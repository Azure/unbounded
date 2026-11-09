//! Per peer and rail circuit breaker from RDMA to HTTP.
//!
//! A link that fails (breaks, fails a write, fails a setup) usually keeps
//! failing for seconds: a hot-removed NIC, a peer reactivating its device, or
//! a QP waiting out its retry count. Every page planned over that link
//! meanwhile waits on a grant, a write, or a deadline before it falls back to
//! HTTP. HTTP carries pages at about the same rate, so the breaker trips on
//! such a failure and, while it is open, sends every page to that peer and
//! rail over HTTP without touching the link.
//!
//! A busy link looks slow, not broken: under full load a 16 MiB page can wait
//! a second or more for a grant slot and its turn on the NIC. So failure
//! signals come in three kinds ([`Severity`]):
//!
//! - Hard (link down, setup failed, a grant or write failed with an error):
//!   the link is broken. Trips at once.
//! - Soft (a page ran out of the page timeout, or the sender answered over
//!   HTTP after it): the link may be stuck, or only congested. Trips after
//!   [`SOFT_TRIP_THRESHOLD`] soft failures in a row with no RDMA success in
//!   between. Any success resets the count; on a congested link pages keep
//!   completing between the slow ones, on a stuck one nothing does.
//! - Load (no grant slot or flight room, ciphertext budget full): the
//!   page goes over HTTP, but the breaker does not trip.
//!
//! States, per `(peer, rail)`:
//!
//! - Closed: RDMA allowed. A hard failure, or enough soft failures in a row,
//!   trip it Open.
//! - Open until a cooldown elapses: every exchange plans HTTP. Further failure
//!   signals do not extend it. The link keeps reconnecting in the background.
//! - Half-open after the cooldown: one probe page may use RDMA at a time. Its
//!   success closes the breaker; its hard or soft failure reopens it with twice the
//!   cooldown, up to the maximum. A probe that never reports (canceled, or its
//!   grant backed off) is replaced after [`Breaker::probe_timeout`].
//!
//! Cooldowns start at `base` and double per consecutive trip up to `max`, with
//! up to a fifth subtracted as jitter so peers do not probe in lockstep. A trip
//! long after the previous one (four times `max`) starts at `base` again.
//!
//! The breaker is node-wide (one per [`super::LinkNode`]) so every worker
//! sees the same verdict, and pure: callers pass the clock.

use std::collections::BTreeMap;
use std::time::{Duration, Instant};

use racer_wire::control::NodeId;
use racer_wire::control::RailId;

/// Soft failures in a row, with no RDMA success in between, that trip a
/// closed breaker.
pub const SOFT_TRIP_THRESHOLD: u32 = 3;

/// An RDMA failure signal; one metric label each.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TripReason {
    /// The link was breaking or backing off after a failure. Hard.
    LinkDown,
    /// A grant on a ready link failed with an error other than load. Hard.
    Grant,
    /// A write, or the arrival of a written page, failed with an error other
    /// than load. Hard.
    Send,
    /// A grant, write, or arrival did not finish within the RDMA page
    /// timeout. Soft.
    Timeout,
    /// The sender answered our grant over HTTP after at least the page
    /// timeout, so its write most likely timed out. Soft.
    SlowFallback,
    /// A link setup this node started failed (not declined). Hard.
    Setup,
    /// No grant slot, flight room, or ciphertext budget for the page. Never
    /// trips.
    Load,
}

/// How a [`TripReason`] counts toward tripping.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Severity {
    /// Trips at once.
    Hard,
    /// Trips after [`SOFT_TRIP_THRESHOLD`] in a row.
    Soft,
    /// Never trips.
    Load,
}

impl TripReason {
    /// Every reason, in metric order.
    pub const ALL: [Self; 7] = [
        Self::LinkDown,
        Self::Grant,
        Self::Send,
        Self::Timeout,
        Self::SlowFallback,
        Self::Setup,
        Self::Load,
    ];

    /// How this reason counts toward tripping.
    pub fn severity(self) -> Severity {
        match self {
            Self::LinkDown | Self::Grant | Self::Send | Self::Setup => Severity::Hard,
            Self::Timeout | Self::SlowFallback => Severity::Soft,
            Self::Load => Severity::Load,
        }
    }

    /// The metric and log label.
    pub fn label(self) -> &'static str {
        match self {
            Self::LinkDown => "link_down",
            Self::Grant => "grant",
            Self::Send => "send",
            Self::Timeout => "timeout",
            Self::SlowFallback => "slow_fallback",
            Self::Setup => "setup",
            Self::Load => "load",
        }
    }
}

/// Breaker tunables.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct BreakerConfig {
    /// `false` keeps every breaker closed.
    pub enabled: bool,

    /// First cooldown after a trip.
    pub base: Duration,

    /// Longest cooldown.
    pub max: Duration,

    /// Longest an RDMA write (sender) or grant (receiver) of one page may
    /// take before the page goes over HTTP, counting a soft failure.
    pub page_timeout: Duration,
}

impl Default for BreakerConfig {
    fn default() -> Self {
        Self {
            enabled: true,
            base: Duration::from_secs(2),
            max: Duration::from_secs(30),
            page_timeout: Duration::from_secs(5),
        }
    }
}

impl Default for Breaker {
    fn default() -> Self {
        Self::new(BreakerConfig::default())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum State {
    Closed,
    Open { until: Instant },
    HalfOpen { probe: Option<Instant> },
}

#[derive(Clone, Copy, Debug)]
struct Entry {
    state: State,
    /// Consecutive trips, for the cooldown exponent.
    trips: u32,
    last_trip: Instant,
    /// Soft failures since the last RDMA success or trip.
    soft: u32,
    /// The last failure signal, for pruning and soft streak expiry.
    touched: Instant,
}

/// What [`Breaker::allow`] decided.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Admit {
    /// Closed: use RDMA.
    Rdma,
    /// Half-open: use RDMA as the probe; report its outcome.
    Probe,
    /// Open, or a probe is already out: use HTTP.
    Http,
}

/// What [`Breaker::trip`] did with a failure signal.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Verdict {
    /// The breaker opened.
    Tripped(Transition),
    /// Counted as a soft or load failure that did not trip.
    Counted,
    /// Ignored: the breaker is disabled or already open.
    Ignored,
}

/// A state change worth a log line.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Transition {
    /// Tripped open for `cooldown`.
    Opened {
        reason: TripReason,
        cooldown: Duration,
        trips: u32,
    },
    /// A probe succeeded.
    Closed,
}

/// Breakers for every `(peer, rail)` this node has seen fail.
#[derive(Debug)]
pub struct Breaker {
    config: BreakerConfig,
    /// By peer, then rail: lookups borrow the peer id instead of cloning it.
    entries: BTreeMap<NodeId, BTreeMap<u16, Entry>>,
    seed: u64,
}

impl Breaker {
    /// Longest a half-open probe may stay out before another is allowed.
    pub fn probe_timeout(&self) -> Duration {
        self.config.base.max(Duration::from_secs(1))
    }

    /// Breakers with `config`, all closed.
    pub fn new(config: BreakerConfig) -> Self {
        Self {
            config,
            entries: BTreeMap::new(),
            seed: 0x9e37_79b9_7f4a_7c15,
        }
    }

    /// Replace the tunables; open breakers keep their cooldowns.
    pub fn configure(&mut self, config: BreakerConfig) {
        self.config = config;
        if !config.enabled {
            self.entries.clear();
        }
    }

    /// The tunables.
    pub fn config(&self) -> BreakerConfig {
        self.config
    }

    /// Whether an exchange with `peer` on `rail` may use RDMA at `now`.
    pub fn allow(&mut self, peer: &NodeId, rail: RailId, now: Instant) -> Admit {
        if !self.config.enabled {
            return Admit::Rdma;
        }
        let probe_timeout = self.probe_timeout();
        let Some(entry) = self.entry(peer, rail) else {
            return Admit::Rdma;
        };
        match entry.state {
            State::Closed => Admit::Rdma,
            State::Open { until } if now < until => Admit::Http,
            State::Open { .. } => {
                entry.state = State::HalfOpen { probe: Some(now) };
                Admit::Probe
            }
            State::HalfOpen { probe: Some(since) }
                if now.saturating_duration_since(since) < probe_timeout =>
            {
                Admit::Http
            }
            State::HalfOpen { .. } => {
                entry.state = State::HalfOpen { probe: Some(now) };
                Admit::Probe
            }
        }
    }

    /// Whether `peer` on `rail` is open (or half-open) at `now`, without
    /// taking a probe.
    pub fn is_open(&self, peer: &NodeId, rail: RailId) -> bool {
        self.entries
            .get(peer)
            .and_then(|rails| rails.get(&rail.0))
            .is_some_and(|e| e.state != State::Closed)
    }

    /// Record an RDMA failure signal for `peer` on `rail` (see
    /// [`Severity`] for which ones trip).
    pub fn trip(
        &mut self,
        peer: &NodeId,
        rail: RailId,
        reason: TripReason,
        now: Instant,
    ) -> Verdict {
        if !self.config.enabled {
            return Verdict::Ignored;
        }
        let severity = reason.severity();
        if severity == Severity::Load {
            // Load says nothing about the link. A probe that met it frees
            // the probe slot so the next page can probe instead.
            if let Some(entry) = self.entry(peer, rail)
                && matches!(entry.state, State::HalfOpen { .. })
            {
                entry.state = State::HalfOpen { probe: None };
            }
            return Verdict::Counted;
        }
        let reset_after = self.config.max * 4;
        let soft_window = self.config.max;
        if !self.entries.contains_key(peer) {
            self.entries.insert(peer.clone(), BTreeMap::new());
        }
        let rails = self.entries.get_mut(peer).expect("inserted above");
        let entry = rails.entry(rail.0).or_insert(Entry {
            state: State::Closed,
            trips: 0,
            last_trip: now,
            soft: 0,
            touched: now,
        });
        if matches!(entry.state, State::Open { .. }) {
            return Verdict::Ignored;
        }
        if entry.state == State::Closed && severity == Severity::Soft {
            // A streak broken by a long quiet spell starts over.
            if now.saturating_duration_since(entry.touched) > soft_window {
                entry.soft = 0;
            }
            entry.touched = now;
            entry.soft = entry.soft.saturating_add(1);
            if entry.soft < SOFT_TRIP_THRESHOLD {
                return Verdict::Counted;
            }
        }
        entry.touched = now;
        entry.soft = 0;
        if entry.state == State::Closed
            && entry.trips > 0
            && now.saturating_duration_since(entry.last_trip) > reset_after
        {
            entry.trips = 0;
        }
        let trips = entry.trips;
        entry.trips = entry.trips.saturating_add(1);
        entry.last_trip = now;
        let full = self
            .config
            .base
            .saturating_mul(1u32.checked_shl(trips.min(16)).unwrap_or(u32::MAX))
            .min(self.config.max)
            .max(self.config.base);
        // Subtract up to a fifth as jitter.
        self.seed ^= self.seed << 13;
        self.seed ^= self.seed >> 7;
        self.seed ^= self.seed << 17;
        let fifth = full.as_nanos() as u64 / 5;
        let jitter = if fifth == 0 { 0 } else { self.seed % fifth };
        let cooldown = full - Duration::from_nanos(jitter);
        entry.state = State::Open {
            until: now + cooldown,
        };
        Verdict::Tripped(Transition::Opened {
            reason,
            cooldown,
            trips: entry.trips,
        })
    }

    /// Record an RDMA success for `peer` on `rail`, ending any soft failure
    /// streak. Returns the transition when this closed a half-open breaker.
    pub fn success(&mut self, peer: &NodeId, rail: RailId) -> Option<Transition> {
        let entry = self.entry(peer, rail)?;
        entry.soft = 0;
        match entry.state {
            State::HalfOpen { .. } => {
                entry.state = State::Closed;
                Some(Transition::Closed)
            }
            _ => None,
        }
    }

    fn entry(&mut self, peer: &NodeId, rail: RailId) -> Option<&mut Entry> {
        self.entries.get_mut(peer)?.get_mut(&rail.0)
    }

    /// Breakers not closed at `now` (open or half-open).
    pub fn open_count(&self) -> usize {
        self.entries
            .values()
            .flat_map(BTreeMap::values)
            .filter(|e| e.state != State::Closed)
            .count()
    }

    /// Forget closed breakers whose last failure is older than the
    /// escalation window, so the table stays bounded by recently failing
    /// peers.
    pub fn prune(&mut self, now: Instant) {
        let window = self.config.max * 4;
        self.entries.retain(|_, rails| {
            rails.retain(|_, e| {
                e.state != State::Closed || now.saturating_duration_since(e.touched) <= window
            });
            !rails.is_empty()
        });
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn peer() -> NodeId {
        NodeId("peer-a".into())
    }

    fn config() -> BreakerConfig {
        BreakerConfig {
            enabled: true,
            base: Duration::from_secs(2),
            max: Duration::from_secs(30),
            page_timeout: Duration::from_secs(2),
        }
    }

    fn opened(t: Verdict) -> Duration {
        match t {
            Verdict::Tripped(Transition::Opened { cooldown, .. }) => cooldown,
            other => panic!("expected a trip, got {other:?}"),
        }
    }

    #[test]
    fn closed_allows_rdma_and_unknown_peers_are_closed() {
        let mut b = Breaker::new(config());
        let now = Instant::now();
        assert_eq!(b.allow(&peer(), RailId(0), now), Admit::Rdma);
        assert_eq!(b.open_count(), 0);
        assert_eq!(b.success(&peer(), RailId(0)), None);
    }

    #[test]
    fn trip_opens_until_cooldown_then_one_probe_closes() {
        let mut b = Breaker::new(config());
        let now = Instant::now();
        let cooldown = opened(b.trip(&peer(), RailId(1), TripReason::Send, now));
        assert!(cooldown <= Duration::from_secs(2) && cooldown >= Duration::from_millis(1600));
        assert_eq!(b.open_count(), 1);
        assert!(b.is_open(&peer(), RailId(1)));
        // Other rails and peers are unaffected.
        assert_eq!(b.allow(&peer(), RailId(0), now), Admit::Rdma);
        assert_eq!(b.allow(&peer(), RailId(1), now), Admit::Http);
        // Repeated failures while open neither extend nor count.
        assert_eq!(
            b.trip(&peer(), RailId(1), TripReason::Timeout, now),
            Verdict::Ignored
        );
        let later = now + cooldown;
        assert_eq!(b.allow(&peer(), RailId(1), later), Admit::Probe);
        // Only one probe at a time.
        assert_eq!(b.allow(&peer(), RailId(1), later), Admit::Http);
        assert_eq!(b.success(&peer(), RailId(1)), Some(Transition::Closed));
        assert_eq!(b.allow(&peer(), RailId(1), later), Admit::Rdma);
        assert_eq!(b.open_count(), 0);
    }

    #[test]
    fn failed_probe_reopens_with_a_longer_cooldown_up_to_max() {
        let mut b = Breaker::new(config());
        let mut now = Instant::now();
        let mut last = Duration::ZERO;
        for round in 0..8 {
            let cooldown = opened(b.trip(&peer(), RailId(0), TripReason::LinkDown, now));
            let full = Duration::from_secs(2 << round).min(Duration::from_secs(30));
            assert!(
                cooldown <= full && cooldown >= full * 4 / 5,
                "{round}: {cooldown:?}"
            );
            if full < Duration::from_secs(30) {
                assert!(cooldown > last);
            }
            last = cooldown;
            now += cooldown;
            assert_eq!(b.allow(&peer(), RailId(0), now), Admit::Probe);
        }
    }

    #[test]
    fn lost_probe_is_replaced_after_the_probe_timeout() {
        let mut b = Breaker::new(config());
        let now = Instant::now();
        let cooldown = opened(b.trip(&peer(), RailId(0), TripReason::Grant, now));
        let probing = now + cooldown;
        assert_eq!(b.allow(&peer(), RailId(0), probing), Admit::Probe);
        let timeout = b.probe_timeout();
        assert_eq!(
            b.allow(&peer(), RailId(0), probing + timeout / 2),
            Admit::Http
        );
        assert_eq!(b.allow(&peer(), RailId(0), probing + timeout), Admit::Probe);
    }

    #[test]
    fn half_open_failure_retrips_and_quiet_peers_start_over() {
        let mut b = Breaker::new(config());
        let now = Instant::now();
        let first = opened(b.trip(&peer(), RailId(0), TripReason::Setup, now));
        let probing = now + first;
        assert_eq!(b.allow(&peer(), RailId(0), probing), Admit::Probe);
        let second = opened(b.trip(&peer(), RailId(0), TripReason::Send, probing));
        assert!(second >= Duration::from_millis(3200));
        let closing = probing + second;
        assert_eq!(b.allow(&peer(), RailId(0), closing), Admit::Probe);
        assert_eq!(b.success(&peer(), RailId(0)), Some(Transition::Closed));
        // Soon after: escalation continues.
        let third = opened(b.trip(&peer(), RailId(0), TripReason::Send, closing));
        assert!(third >= Duration::from_millis(6400));
        let reopened = closing + third;
        assert_eq!(b.allow(&peer(), RailId(0), reopened), Admit::Probe);
        b.success(&peer(), RailId(0));
        // Long after: back to the base cooldown.
        let quiet = reopened + Duration::from_secs(121);
        let fresh = opened(b.trip(&peer(), RailId(0), TripReason::Send, quiet));
        assert!(fresh <= Duration::from_secs(2));
        b.prune(quiet);
        assert_eq!(b.open_count(), 1);
    }

    #[test]
    /// Rails of one peer trip, close, and prune independently.
    fn rails_of_one_peer_are_independent() {
        let mut b = Breaker::new(config());
        let now = Instant::now();
        opened(b.trip(&peer(), RailId(0), TripReason::Send, now));
        assert!(b.is_open(&peer(), RailId(0)));
        assert!(!b.is_open(&peer(), RailId(1)));
        assert_eq!(b.allow(&peer(), RailId(1), now), Admit::Rdma);
        assert_eq!(
            b.trip(&peer(), RailId(1), TripReason::Timeout, now),
            Verdict::Counted
        );
        assert_eq!(b.success(&peer(), RailId(1)), None);
        let later = now + Duration::from_secs(121);
        b.prune(later);
        assert_eq!(b.open_count(), 1);
        assert_eq!(
            b.entries[&peer()].len(),
            1,
            "the quiet closed rail is pruned"
        );
        assert_eq!(b.allow(&peer(), RailId(0), later), Admit::Probe);
        assert_eq!(b.success(&peer(), RailId(0)), Some(Transition::Closed));
        b.prune(later + Duration::from_secs(121));
        assert!(b.entries.is_empty());
    }

    #[test]
    fn disabled_never_trips_and_prune_drops_quiet_closed_entries() {
        let mut b = Breaker::new(BreakerConfig {
            enabled: false,
            ..config()
        });
        let now = Instant::now();
        assert_eq!(
            b.trip(&peer(), RailId(0), TripReason::Send, now),
            Verdict::Ignored
        );
        assert_eq!(b.allow(&peer(), RailId(0), now), Admit::Rdma);
        b.configure(config());
        let cooldown = opened(b.trip(&peer(), RailId(0), TripReason::Send, now));
        b.allow(&peer(), RailId(0), now + cooldown);
        b.success(&peer(), RailId(0));
        b.prune(now + Duration::from_secs(121));
        assert!(b.entries.is_empty());
    }

    #[test]
    fn severities_split_hard_soft_and_load() {
        use Severity::*;
        let kinds: Vec<_> = TripReason::ALL.iter().map(|r| r.severity()).collect();
        assert_eq!(kinds, [Hard, Hard, Hard, Soft, Soft, Hard, Load]);
    }

    #[test]
    fn every_hard_failure_trips_at_once() {
        for reason in TripReason::ALL {
            if reason.severity() != Severity::Hard {
                continue;
            }
            let mut b = Breaker::new(config());
            opened(b.trip(&peer(), RailId(0), reason, Instant::now()));
            assert!(b.is_open(&peer(), RailId(0)), "{reason:?}");
        }
    }

    #[test]
    fn soft_failures_trip_only_after_a_streak_without_success() {
        let mut b = Breaker::new(config());
        let now = Instant::now();
        for reason in [TripReason::Timeout, TripReason::SlowFallback] {
            for _ in 1..SOFT_TRIP_THRESHOLD {
                assert_eq!(b.trip(&peer(), RailId(0), reason, now), Verdict::Counted);
                assert_eq!(b.allow(&peer(), RailId(0), now), Admit::Rdma);
            }
            // A success ends the streak.
            assert_eq!(b.success(&peer(), RailId(0)), None);
        }
        assert_eq!(b.open_count(), 0);
        // Mixed soft reasons count toward one streak.
        assert_eq!(
            b.trip(&peer(), RailId(0), TripReason::Timeout, now),
            Verdict::Counted
        );
        assert_eq!(
            b.trip(&peer(), RailId(0), TripReason::SlowFallback, now),
            Verdict::Counted
        );
        let cooldown = opened(b.trip(&peer(), RailId(0), TripReason::Timeout, now));
        assert!(cooldown <= Duration::from_secs(2));
        assert_eq!(b.allow(&peer(), RailId(0), now), Admit::Http);
    }

    #[test]
    fn a_soft_streak_expires_after_a_quiet_spell() {
        let mut b = Breaker::new(config());
        let mut now = Instant::now();
        for _ in 0..5 {
            assert_eq!(
                b.trip(&peer(), RailId(0), TripReason::Timeout, now),
                Verdict::Counted
            );
            now += Duration::from_secs(31);
        }
        assert_eq!(b.open_count(), 0);
    }

    #[test]
    fn load_never_trips_and_frees_a_stuck_probe() {
        let mut b = Breaker::new(config());
        let now = Instant::now();
        for _ in 0..10 {
            assert_eq!(
                b.trip(&peer(), RailId(0), TripReason::Load, now),
                Verdict::Counted
            );
        }
        assert_eq!(b.allow(&peer(), RailId(0), now), Admit::Rdma);
        assert!(b.entries.is_empty());
        let cooldown = opened(b.trip(&peer(), RailId(0), TripReason::Send, now));
        let probing = now + cooldown;
        assert_eq!(b.allow(&peer(), RailId(0), probing), Admit::Probe);
        assert_eq!(b.allow(&peer(), RailId(0), probing), Admit::Http);
        assert_eq!(
            b.trip(&peer(), RailId(0), TripReason::Load, probing),
            Verdict::Counted
        );
        assert_eq!(b.allow(&peer(), RailId(0), probing), Admit::Probe);
        assert_eq!(b.success(&peer(), RailId(0)), Some(Transition::Closed));
    }

    #[test]
    fn a_soft_failure_of_the_probe_reopens_at_once() {
        let mut b = Breaker::new(config());
        let now = Instant::now();
        let first = opened(b.trip(&peer(), RailId(0), TripReason::LinkDown, now));
        let probing = now + first;
        assert_eq!(b.allow(&peer(), RailId(0), probing), Admit::Probe);
        let second = opened(b.trip(&peer(), RailId(0), TripReason::Timeout, probing));
        assert!(second >= Duration::from_millis(3200));
        // Half-open recovery: the next probe's success closes it and later
        // soft failures need a fresh streak.
        let closing = probing + second;
        assert_eq!(b.allow(&peer(), RailId(0), closing), Admit::Probe);
        assert_eq!(b.success(&peer(), RailId(0)), Some(Transition::Closed));
        assert_eq!(
            b.trip(&peer(), RailId(0), TripReason::Timeout, closing),
            Verdict::Counted
        );
        assert_eq!(b.allow(&peer(), RailId(0), closing), Admit::Rdma);
    }
}
