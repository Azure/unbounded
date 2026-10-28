//! Control plane integration: the real `racer_cluster_control` model driven
//! on the simulator timeline, plus a per-client enrollment and long-poll
//! model built from the real seat server admission steps.
//!
//! [`ControlSim`] co-steps two queues on one clock: the
//! [`ControlPlane`] model (enrollment lottery, seats, primary, commits) and
//! an [`Engine`] of client events. [`ControlSim::advance_to`] always runs
//! whichever is earlier, the control plane first on ties, so control and
//! client events share one deterministic timeline and one trace hash. A
//! data-plane world can co-step a `ControlSim` the same way, using
//! [`ControlSim::next_event_ns`] and [`ControlSim::advance_to`], and read
//! [`ControlSim::membership_version`] per node so its nodes lag behind the
//! committed membership exactly as their control clients do.
//!
//! Client model (one client per Pod, structure of arrays):
//! - Enrollment takes a bootstrap slot on a ready seat
//!   ([`SeatAdmission::bootstraps`], 32 per seat), then performs the bearer
//!   authorization API calls (`server.rs` `authorize_bearer` and `auth.rs`
//!   `authorize`): one TokenReview POST and four GETs (Pod, DaemonSet,
//!   ServiceAccount, Node) through [`Store`] with a [`TokenBucket`]; a 429
//!   releases the slot and backs off.
//! - Snapshot long polls use [`SeatAdmission::snapshot`] (one poll per
//!   node, [`check_ahead`] 503 for a client ahead of its seat), a write slot
//!   ([`SeatAdmission::writes`], 64 per seat, then a FIFO queue bounded by
//!   `limits::MAX_WRITE_WAITERS`; a poll still queued past its poll deadline
//!   plus `limits::WRITE_QUEUE_GRACE` answers 429), and [`body_for`] for
//!   delta versus full bodies. Waiters wake when their seat serves a new
//!   sequence; the wake costs one waker registration per waiter (O(W)).
//! - Each client holds the content hash of the image it applied (none
//!   after a process restart) and sends it with its applied sequence, as
//!   production clients do; `body_for` answers a delta only when both
//!   match the image's delta base, else the full image.
//!
//! Faults: crash the primary or seat holders, restart, partition a fraction
//! of clients from the control plane, slow clients, grow and shrink.

use std::{
    cmp::Reverse,
    collections::{BTreeMap, BinaryHeap},
    time::{Duration, Instant, SystemTime},
};

use racer_cluster_control::{
    PrimaryView, SeatPolicy, annotations,
    apiserver::{Store, TokenBucket},
    authority::{
        election::{self, LeaseObservation, LeaseSpec},
        keyring::Policy,
        members::{NodeInfo, Owner, Ownership, PodInfo, Readiness},
        publication::Image,
        state::{self, StateRecord, Term, Writer},
    },
    limits,
    step::{
        admission::{Admit, Body, Poll, SeatAdmission, WriteWaiter, body_for},
        authz::{ApiBudget, AuthCache, Bindings, Reviewed, TokenKey},
        primary::{Contend, Inventory, PassInputs, discovery, plan_pass},
        seat::{self as rules, Follower, Lottery, SeatRenew},
    },
};
use racer_control_wire::ProtocolFailure;

use racer_core::time::SplitMix64;
use serde_json::{Value, json};

use crate::engine::{Engine, MILLIS, Nanos, Role, SECONDS, Streams, mix};
use crate::report::{Invariants, Metrics, peak_rss_bytes};

/// Nonzero fingerprint of a hex content hash (its first 64 bits).
fn fingerprint(hash: &str) -> u64 {
    let head = hash.get(..16).unwrap_or(hash);
    u64::from_str_radix(head, 16).unwrap_or_else(|_| mix(hash.len() as u64)) | 1
}

/// A queued write waiter still wants its slot at `now`: same connection
/// epoch, still polling, deadline not passed. Connected waiters whose
/// deadline passed are collected so the caller can answer them 429.
fn expire(
    epoch: &[u32],
    state: &[State],
    w: &WriteWaiter,
    now: Nanos,
    expired: &mut Vec<u32>,
) -> bool {
    let connected =
        epoch[w.node as usize] == w.epoch && matches!(state[w.node as usize], State::Polling);
    if connected && w.deadline_ns < now {
        expired.push(w.node);
    }
    connected && w.deadline_ns >= now
}

/// The body a seat sends a client that applied `after` and holds the
/// content hash with fingerprint `held` (0: none), for an image whose delta
/// base is `base`. The client sends the hash it actually holds; a different
/// held hash is any other string to `body_for`.
fn reply_body(base: Option<(u64, &str)>, after: Option<u64>, held: u64) -> Body {
    let sent = (held != 0).then(|| match base {
        Some((_, h)) if fingerprint(h) == held => h,
        _ => "",
    });
    body_for(base, after, sent)
}

/// Control scenario names accepted by the CLI.
pub const SCENARIOS: &[&str] = &["enrollment-storm", "failover", "ctl-churn", "lagging"];

/// Model parameters. JSON overrides live under `"control"` in `--config`.
#[derive(Clone, Debug)]
pub struct Settings {
    /// Pods (and clients) at the end of growth.
    pub pods: u32,
    /// Pods present at time zero (the rest join in waves).
    pub initial_pods: u32,
    /// Clients start enrolling uniformly within this window.
    pub enroll_spread_ns: Nanos,
    /// Kubernetes API server rate limit (all clients share it).
    pub api_qps: f64,
    pub api_burst: f64,
    pub api_latency_ns: Nanos,
    /// Per write slot throughput of a seat, bytes per second.
    pub slot_bps: u64,
    /// Fixed cost of one reply write.
    pub write_ns: Nanos,
    /// TLS and enrollment CPU on the seat.
    pub handshake_ns: Nanos,
    /// Client retry backoff: base and cap (doubling, with jitter).
    pub backoff_ns: Nanos,
    pub backoff_cap_ns: Nanos,
    /// Time a partitioned client waits before its connection attempt fails.
    pub connect_timeout_ns: Nanos,
    /// Extra delay before a slow client polls again after a reply.
    pub slow_delay_ns: Nanos,
    /// Invariant check period and convergence bound after faults heal.
    pub check_ns: Nanos,
    pub converge_ns: Nanos,
    pub image_bytes_per_member: u64,
    /// Crash to Node `Ready=Unknown` (Kubernetes `node-monitor-grace-period`);
    /// 0 never marks crashed Nodes (the pre-round-4 model).
    pub node_monitor_grace_ns: Nanos,
    /// Seat authorization model, see [`AuthModel`].
    pub auth: AuthModel,
    /// Per-seat authorization API budget (`limits::AUTH_API_QPS`, `_BURST`)
    /// for [`AuthModel::Cached`].
    pub auth_qps: f64,
    pub auth_burst: f64,
    /// Pods report from creation, before enrolling (the pre-round-4
    /// dataplane), so they join the membership before they can serve.
    /// False (the default) reports when the client first applies a
    /// publication, which is when production opens its peer listener.
    pub report_at_start: bool,
}

/// How seats authorize an enrolling client's bearer token.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AuthModel {
    /// Before round 4: the client's first bearer keyring poll authorizes
    /// twice (once on entry, again because data is about to leave), then
    /// the bootstrap authorizes again: three full authorizations of five
    /// API calls each, no cache, no budget. An API 429 part way wastes the
    /// calls already made.
    Legacy,
    /// Round 4 (`step::authz`): one keyring authorization (data found
    /// without waiting needs no recheck) and one bootstrap authorization,
    /// both through the seat's `AuthCache` (a whole authorization reused
    /// for `CALLER_TTL`, TokenReview for `REVIEW_TTL`, DaemonSet and
    /// ServiceAccount for `BINDINGS_TTL`) and its adaptive `ApiBudget`.
    Cached,
}

impl Default for Settings {
    fn default() -> Self {
        Self {
            pods: 200,
            initial_pods: 200,
            enroll_spread_ns: 10 * SECONDS,
            api_qps: 1000.0,
            api_burst: 2000.0,
            api_latency_ns: 5 * MILLIS,
            // 10 Gbit/s seat NIC shared by 64 write slots.
            slot_bps: 1_250_000_000 / limits::MAX_WRITES as u64,
            write_ns: 200_000,
            handshake_ns: 2 * MILLIS,
            backoff_ns: SECONDS,
            backoff_cap_ns: 30 * SECONDS,
            connect_timeout_ns: limits::IO_TIMEOUT.as_nanos() as u64,
            slow_delay_ns: 20 * SECONDS,
            check_ns: 10 * SECONDS,
            converge_ns: 180 * SECONDS,
            image_bytes_per_member: 128,
            node_monitor_grace_ns: 40 * SECONDS,
            auth: AuthModel::Cached,
            auth_qps: limits::AUTH_API_QPS,
            auth_burst: limits::AUTH_API_BURST,
            report_at_start: false,
        }
    }
}

impl Settings {
    /// Authorizations per enrollment and the most API calls they make.
    fn enrollment_calls(&self) -> (u8, u64) {
        match self.auth {
            AuthModel::Legacy => (3, 15),
            // Cold seat: 5 for the first, none for the second within
            // `CALLER_TTL` on the same seat; 10 if it moves seats.
            AuthModel::Cached => (2, 10),
        }
    }

    /// Apply a JSON object of overrides.
    pub fn apply_json(&mut self, value: &Value) -> Result<(), String> {
        let obj = value
            .as_object()
            .ok_or_else(|| format!("control: expected an object, got {value}"))?;
        for (k, v) in obj {
            let u = || {
                v.as_u64()
                    .ok_or_else(|| format!("control.{k}: expected a non-negative integer"))
            };
            let f = || {
                v.as_f64()
                    .filter(|f| f.is_finite() && *f > 0.0)
                    .ok_or_else(|| format!("control.{k}: expected a positive number"))
            };
            match k.as_str() {
                "initial_pods" => {
                    self.initial_pods =
                        u32::try_from(u()?).map_err(|_| format!("control.{k}: out of range"))?
                }
                "enroll_spread_ns" => self.enroll_spread_ns = u()?,
                "api_qps" => self.api_qps = f()?,
                "api_burst" => self.api_burst = f()?,
                "api_latency_ns" => self.api_latency_ns = u()?,
                "slot_bps" => self.slot_bps = u()?.max(1),
                "write_ns" => self.write_ns = u()?,
                "handshake_ns" => self.handshake_ns = u()?,
                "backoff_ns" => self.backoff_ns = u()?.max(1),
                "backoff_cap_ns" => self.backoff_cap_ns = u()?.max(1),
                "connect_timeout_ns" => self.connect_timeout_ns = u()?,
                "slow_delay_ns" => self.slow_delay_ns = u()?,
                "check_ns" => self.check_ns = u()?.max(MILLIS),
                "converge_ns" => self.converge_ns = u()?,
                "image_bytes_per_member" => self.image_bytes_per_member = u()?,
                "node_monitor_grace_ns" => self.node_monitor_grace_ns = u()?,
                "auth" => {
                    self.auth = match v.as_str() {
                        Some("legacy") => AuthModel::Legacy,
                        Some("cached") => AuthModel::Cached,
                        _ => return Err(format!("control.{k}: expected \"legacy\" or \"cached\"")),
                    }
                }
                "auth_qps" => self.auth_qps = f()?,
                "auth_burst" => self.auth_burst = f()?,
                "report_at_start" => {
                    self.report_at_start = v
                        .as_bool()
                        .ok_or_else(|| format!("control.{k}: expected a boolean"))?
                }
                _ => return Err(format!("unknown control key {k:?}")),
            }
        }
        Ok(())
    }
}

/// A scheduled control-plane fault or membership change.
#[derive(Clone, Debug, PartialEq)]
pub enum CtlFault {
    /// Crash the current primary; restart it after `restart_after_ns`.
    CrashPrimary {
        at_ns: Nanos,
        restart_after_ns: Option<Nanos>,
    },
    /// Crash up to `count` seat holders that are not the primary.
    CrashSeats {
        at_ns: Nanos,
        count: u32,
        restart_after_ns: Option<Nanos>,
    },
    /// Cut `fraction` of clients off from the control plane.
    Partition {
        from_ns: Nanos,
        until_ns: Nanos,
        fraction: f64,
    },
    /// Make `fraction` of clients slow to poll again.
    Slow {
        from_ns: Nanos,
        until_ns: Nanos,
        fraction: f64,
    },
    /// Add `count` Pods.
    Grow { at_ns: Nanos, count: u32 },
    /// Remove `count` live Pods (never the primary).
    Shrink { at_ns: Nanos, count: u32 },
}

impl CtlFault {
    fn at(&self) -> Nanos {
        match self {
            Self::CrashPrimary { at_ns, .. }
            | Self::CrashSeats { at_ns, .. }
            | Self::Grow { at_ns, .. }
            | Self::Shrink { at_ns, .. } => *at_ns,
            Self::Partition { from_ns, .. } | Self::Slow { from_ns, .. } => *from_ns,
        }
    }

    /// When the fault stops disturbing the cluster (for convergence).
    fn heals_at(&self) -> Nanos {
        match self {
            Self::CrashPrimary {
                at_ns,
                restart_after_ns,
            }
            | Self::CrashSeats {
                at_ns,
                restart_after_ns,
                ..
            } => at_ns + restart_after_ns.unwrap_or(0),
            Self::Partition { until_ns, .. } | Self::Slow { until_ns, .. } => *until_ns,
            Self::Grow { at_ns, .. } | Self::Shrink { at_ns, .. } => *at_ns,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Ev {
    Enroll(u32, u32),
    Enrolled(u32, u32, u8),
    Poll(u32, u32),
    /// Client, epoch, seat, sequence, membership version, and the
    /// fingerprint of the delivered image's content hash.
    Delivered(u32, u32, u8, u64, u64, u64),
    Fault(u32),
    Heal(u32),
    Restart(u32),
    Check,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(u8)]
enum State {
    Idle,
    Enrolling,
    Polling,
    Gone,
}

const NO_SEAT: u8 = u8::MAX;

struct Seat {
    admission: SeatAdmission,
    served: Option<u64>,
    waiters: Vec<(u32, u32)>,
    /// The holder process's authorization cache and API budget; a new
    /// holder starts with fresh ones.
    auth: AuthCache,
    budget: ApiBudget,
}

/// Token cache key of client `c` (production hashes the token).
fn client_key(c: u32) -> TokenKey {
    let mut k = [0u8; 32];
    k[..4].copy_from_slice(&c.to_le_bytes());
    k
}

/// A Pod lifecycle change, for a co-stepped data plane.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Life {
    /// The Pod process crashed (its disk survives).
    Crashed,
    /// The Pod process restarted.
    Restarted,
    /// The Pod and its Node were removed from the cluster.
    Removed,
}

/// Changes since a co-stepped data plane last took them, recorded only
/// when [`ControlSim::record`] is on.
#[derive(Debug, Default)]
pub struct Changes {
    /// Newly committed membership versions with their member Pods.
    pub memberships: Vec<(u64, Vec<u32>)>,
    /// `(pod, membership version)` applied by its client (0 on restart).
    pub applied: Vec<(u32, u64)>,
    pub lives: Vec<(u32, Life)>,
}

/// Pod index of a published member: the model names Node `i` with the UID
/// `{i:08x}-0000-...` (`cluster` `uuid`).
fn member_pod(node: &str) -> Option<u32> {
    u32::from_str_radix(node.get(..8)?, 16).ok()
}

/// Control plane plus clients on one deterministic timeline.
pub struct ControlSim {
    pub settings: Settings,
    pub(crate) cp: ControlPlane,
    engine: Engine<Ev>,
    api: Store,
    seats: Vec<Seat>,
    rng: SplitMix64,
    // Clients, structure of arrays, indexed by Pod.
    state: Vec<State>,
    epoch: Vec<u32>,
    seat: Vec<u8>,
    attempts: Vec<u8>,
    /// Authorizations of the current enrollment already granted.
    authorized: Vec<u8>,
    applied_seq: Vec<u64>,
    applied_mv: Vec<u64>,
    /// Fingerprint of the content hash each client holds (0: none).
    held: Vec<u64>,
    partitioned: Vec<bool>,
    slow: Vec<bool>,
    started_ns: Vec<Nanos>,
    /// When each client's current long poll started (its poll deadline is
    /// a whole number of `POLL_WAIT`s later, as the client re-polls on 204).
    poll_ns: Vec<Nanos>,
    faults: Vec<CtlFault>,
    /// Time after which every live client must converge.
    settled_ns: Nanos,
    commits: Vec<(Nanos, u64, u64)>,
    /// First time any client applied each membership version (for the
    /// adoption spread, `ctl_adopt_spread_ns`).
    first_adopt: std::collections::HashMap<u64, Nanos>,
    /// Content hash fingerprint per committed sequence.
    hashes: std::collections::HashMap<u64, u64>,
    last_primary: Option<u32>,
    primary_lost_ns: Option<Nanos>,
    last_terms: u64,
    last_dual: u64,
    cp_api: (u64, u64),
    qps_second: u64,
    qps_count: u64,
    /// Monotonic origin of the simulated clock for `step::authz`.
    base: Instant,
    enrolled: u32,
    live_clients: u32,
    /// Record membership commits, applied versions and Pod lifecycle
    /// changes in `changes`.
    pub(crate) record: bool,
    pub(crate) changes: Changes,
    pub metrics: Metrics,
    pub invariants: Invariants,
}

impl ControlSim {
    pub fn new(settings: Settings, seed: u64, faults: Vec<CtlFault>) -> Self {
        let initial = settings.initial_pods.clamp(1, settings.pods.max(1));
        let mut cp = ControlPlane::new(initial, seed, settings.report_at_start);
        cp.image_bytes_per_member = settings.image_bytes_per_member;
        cp.node_monitor_grace = (settings.node_monitor_grace_ns > 0)
            .then(|| Duration::from_nanos(settings.node_monitor_grace_ns));
        let mut api = Store::new();
        api.set_admission(Some(TokenBucket::new(
            settings.api_qps,
            settings.api_burst,
            settings.api_latency_ns,
        )));
        api.on_post(|path, _| {
            path.ends_with("/tokenreviews")
                .then(|| (201, json!({"status": {"authenticated": true}})))
        });
        api.put_object(DAEMONSET, json!({"metadata": {"name": "racer-dataplane"}}));
        api.put_object(
            SERVICE_ACCOUNT,
            json!({"metadata": {"name": "racer-dataplane"}}),
        );
        let mut sim = Self {
            rng: Streams::new(seed).stream(Role::Control, 0),
            cp,
            engine: Engine::new(),
            api,
            seats: (0..annotations::MAX_SEATS)
                .map(|_| Seat {
                    admission: SeatAdmission::default(),
                    served: None,
                    waiters: Vec::new(),
                    auth: AuthCache::default(),
                    budget: ApiBudget::new(settings.auth_qps, settings.auth_burst),
                })
                .collect(),
            state: Vec::new(),
            epoch: Vec::new(),
            seat: Vec::new(),
            attempts: Vec::new(),
            authorized: Vec::new(),
            applied_seq: Vec::new(),
            base: Instant::now(),
            applied_mv: Vec::new(),
            held: Vec::new(),
            partitioned: Vec::new(),
            slow: Vec::new(),
            started_ns: Vec::new(),
            poll_ns: Vec::new(),
            settled_ns: faults.iter().map(CtlFault::heals_at).max().unwrap_or(0),
            faults,
            commits: Vec::new(),
            first_adopt: std::collections::HashMap::new(),
            hashes: std::collections::HashMap::new(),
            last_primary: None,
            primary_lost_ns: None,
            last_terms: 0,
            last_dual: 0,
            cp_api: (0, 0),
            qps_second: 0,
            qps_count: 0,
            enrolled: 0,
            live_clients: 0,
            record: false,
            changes: Changes::default(),
            metrics: Metrics::default(),
            invariants: Invariants::default(),
            settings,
        };
        for _ in 0..initial {
            sim.add_client();
        }
        for i in 0..sim.faults.len() {
            let at = sim.faults[i].at();
            sim.engine.at(at, Ev::Fault(i as u32));
        }
        sim.engine.at(sim.settings.check_ns, Ev::Check);
        sim
    }

    fn add_client(&mut self) -> u32 {
        let c = self.state.len() as u32;
        let p = self.cp.pods.len() as u32;
        let c = if p > c { c } else { self.cp.add_pod() };
        debug_assert_eq!(c as usize, self.state.len());
        self.api
            .put_object(&pod_path(c), json!({"metadata": {"name": format!("p{c}")}}));
        self.api.put_object(
            &node_path(c),
            json!({"metadata": {"name": format!("n{c}")}}),
        );
        self.state.push(State::Idle);
        self.epoch.push(0);
        self.seat.push(NO_SEAT);
        self.attempts.push(0);
        self.authorized.push(0);
        self.applied_seq.push(0);
        self.applied_mv.push(0);
        self.held.push(0);
        self.partitioned.push(false);
        self.slow.push(false);
        self.started_ns.push(self.engine.now());
        self.poll_ns.push(0);
        self.live_clients += 1;
        let spread = self.settings.enroll_spread_ns.max(1);
        let delay = self.rng.next_u64() % spread;
        self.engine.after(delay, Ev::Enroll(c, 0));
        c
    }

    /// Membership version the client on `node` has applied (0 before its
    /// first snapshot). Data-plane nodes use this to lag like their client.
    pub fn membership_version(&self, node: u32) -> u64 {
        self.applied_mv.get(node as usize).copied().unwrap_or(0)
    }

    /// The committed membership version, if any.
    pub fn committed_membership_version(&self) -> Option<u64> {
        self.cp.record.as_ref().map(|r| r.membership_version)
    }

    /// Current time.
    pub fn now_ns(&self) -> Nanos {
        self.engine.now().max(self.cp.now)
    }

    /// Next pending event of either queue.
    pub fn next_event_ns(&self) -> Option<Nanos> {
        match (self.engine.next_at(), self.cp.next_event_ns()) {
            (Some(a), Some(b)) => Some(a.min(b)),
            (a, b) => a.or(b),
        }
    }

    /// Order-sensitive digest of control and client events so far.
    pub fn trace_hash(&self) -> u64 {
        self.engine.trace.value()
    }

    /// Run every event up to and including `until`.
    pub fn advance_to(&mut self, until: Nanos) {
        loop {
            let a = self.engine.next_at().filter(|t| *t <= until);
            let b = self.cp.next_event_ns().filter(|t| *t <= until);
            match (a, b) {
                (None, None) => break,
                (a, Some(b)) if a.is_none_or(|a| b <= a) => {
                    self.cp.advance_to(b);
                    self.sync(b);
                }
                _ => {
                    let ev = self.engine.pop_until(until).expect("peeked");
                    self.handle(ev);
                }
            }
        }
        self.cp.advance_to(until);
        self.sync(until);
        self.engine.finish(until);
    }

    fn trace(&mut self, at: Nanos, tag: u64, a: u64, b: u64) {
        self.engine.trace.record(at, tag, a, b);
    }

    fn count_api(&mut self, at: Nanos, n: u64) {
        let second = at / SECONDS;
        if second != self.qps_second {
            self.qps_second = second;
            self.qps_count = 0;
        }
        self.qps_count += n;
        let peak = self.metrics.get("ctl_api_qps_peak");
        if self.qps_count > peak {
            self.metrics.add("ctl_api_qps_peak", self.qps_count - peak);
        }
    }

    /// Fold control-plane changes after the model ran to `at`.
    fn sync(&mut self, at: Nanos) {
        let s = self.cp.stats.clone();
        let reads = s.api_reads - self.cp_api.0;
        let writes = s.api_writes - self.cp_api.1;
        if reads + writes > 0 {
            self.cp_api = (s.api_reads, s.api_writes);
            self.metrics.add("ctl_api_lease_reads", reads);
            self.metrics.add("ctl_api_lease_writes", writes);
            self.count_api(at, reads + writes);
        }
        if s.primary_terms != self.last_terms {
            let ok = s.primary_terms > self.last_terms;
            self.invariants
                .check("ctl_terms_monotonic", ok, at, None, || {
                    format!("terms {} -> {}", self.last_terms, s.primary_terms)
                });
            self.last_terms = s.primary_terms;
            self.trace(at, 0xC1, s.primary_terms, 0);
        }
        self.invariants.check(
            "ctl_single_primary",
            s.dual_primary == self.last_dual,
            at,
            self.cp.primary.holder,
            || format!("dual_primary {} -> {}", self.last_dual, s.dual_primary),
        );
        self.last_dual = s.dual_primary;
        // Primary availability: lost when its Pod dies, back on a new term.
        let primary = self.cp.primary.holder.filter(|p| self.cp.alive(*p));
        if primary != self.last_primary {
            match (self.last_primary, primary) {
                (Some(_), None) => self.primary_lost_ns = Some(at),
                (_, Some(p)) => {
                    if let Some(lost) = self.primary_lost_ns.take() {
                        self.metrics.observe("ctl_failover_ns", at - lost);
                    }
                    self.trace(at, 0xC2, u64::from(p), 0);
                }
                (None, None) => {}
            }
            self.last_primary = primary;
        }
        if let Some(r) = self.cp.record.as_ref() {
            let (seq, mv) = (r.sequence, r.membership_version);
            self.hashes
                .entry(seq)
                .or_insert_with(|| fingerprint(&r.content_hash));
            let last = self.commits.last().copied();
            if last.is_none_or(|(_, s, _)| s != seq) {
                let ok = last.is_none_or(|(_, s, m)| seq > s && mv >= m);
                self.invariants
                    .check("ctl_sequence_monotonic", ok, at, None, || {
                        format!("{last:?} -> ({seq}, {mv})")
                    });
                let commit = self.cp.commit_at.get(&seq).copied().unwrap_or(at);
                if let Some((t, _, m)) = last {
                    self.metrics.observe("ctl_commit_interval_ns", commit - t);
                    if mv != m {
                        self.metrics.incr("ctl_membership_versions");
                    }
                } else {
                    self.metrics.incr("ctl_membership_versions");
                }
                if self.record && last.is_none_or(|(_, _, m)| m != mv) {
                    match self.cp.image.as_ref().filter(|i| i.record.sequence == seq) {
                        Some(i) => {
                            let pods = i
                                .publication
                                .members
                                .iter()
                                .filter_map(|m| member_pod(&m.node.0))
                                .collect();
                            self.changes.memberships.push((mv, pods));
                        }
                        None => self.metrics.incr("ctl_membership_without_image"),
                    }
                }
                self.metrics.incr("ctl_sequences");
                let bytes = r.publication_bytes;
                self.metrics.add("ctl_publication_bytes", bytes);
                self.metrics.observe("ctl_publication_bytes", bytes);
                if let Some(d) = self
                    .cp
                    .image
                    .as_ref()
                    .and_then(|i| i.delta.as_ref())
                    .filter(|_| {
                        self.cp
                            .image
                            .as_ref()
                            .is_some_and(|i| i.record.sequence == seq)
                    })
                {
                    self.metrics.observe("ctl_delta_bytes", d.2.len() as u64);
                }
                self.commits.push((commit, seq, mv));
                self.trace(at, 0xC3, seq, mv);
            }
        }
        for i in 0..self.seats.len() {
            let served = self.cp.seat_served(i);
            if served == self.seats[i].served {
                continue;
            }
            let previous = self.seats[i].served;
            self.seats[i].served = served;
            if let Some(t) = served
                .filter(|_| previous.is_some())
                .and_then(|q| self.cp.commit_at.get(&q).copied())
            {
                // Commit to this seat serving it (follower read cadence).
                self.metrics
                    .observe("ctl_seat_lag_ns", at.saturating_sub(t));
            }
            self.trace(at, 0xC4, i as u64, served.unwrap_or(u64::MAX));
            let waiters = std::mem::take(&mut self.seats[i].waiters);
            // One O(1) waker registration per waiter (`exec::Changed`).
            self.metrics
                .add("ctl_notify_registrations", waiters.len() as u64);
            match served {
                Some(_) if previous.is_some() => {
                    // New sequence: every live waiter replies now.
                    for (c, e) in waiters {
                        if self.epoch[c as usize] == e && self.state[c as usize] == State::Polling {
                            self.reply(c, i as u8, at);
                        }
                    }
                }
                _ => {
                    // Seat lost or new holder: connections drop, clients
                    // reconnect (possibly to another seat). Polls queued
                    // for a write slot drop too.
                    if served.is_none() || previous.is_none() {
                        // A new holder process: cold cache, full budget.
                        self.seats[i].auth = AuthCache::default();
                        self.seats[i].budget =
                            ApiBudget::new(self.settings.auth_qps, self.settings.auth_burst);
                    }
                    let queued = self.seats[i].admission.writes.take_waiting();
                    let waiters = waiters
                        .into_iter()
                        .chain(queued.into_iter().map(|w| (w.node, w.epoch)));
                    for (c, e) in waiters {
                        self.seats[i].admission.end_poll(c, false);
                        if self.epoch[c as usize] == e {
                            self.engine.at(at, Ev::Poll(c, e));
                        }
                    }
                }
            }
        }
    }

    fn ready_seats(&self) -> impl Iterator<Item = u8> + '_ {
        self.seats
            .iter()
            .enumerate()
            .filter(|(_, s)| s.served.is_some())
            .map(|(i, _)| i as u8)
    }

    /// Stable seat choice for client `c`, skipping `avoid`.
    fn pick_seat(&self, c: u32, avoid: u8) -> Option<u8> {
        let ready: Vec<u8> = self.ready_seats().filter(|s| *s != avoid).collect();
        if ready.is_empty() {
            return None;
        }
        let r = mix(u64::from(c) ^ (u64::from(self.attempts[c as usize]) << 40));
        Some(ready[(r % ready.len() as u64) as usize])
    }

    fn backoff(&mut self, c: u32) -> Nanos {
        let a = self.attempts[c as usize].min(16);
        self.attempts[c as usize] = self.attempts[c as usize].saturating_add(1);
        let base = (self.settings.backoff_ns << a).min(self.settings.backoff_cap_ns);
        base / 2 + self.rng.next_u64() % (base / 2).max(1)
    }

    fn handle(&mut self, ev: Ev) {
        let now = self.engine.now();
        match ev {
            Ev::Enroll(c, e) if self.live(c, e) => self.enroll(c, e, now),
            Ev::Enrolled(c, e, s) if self.live(c, e) => {
                self.seats[s as usize].admission.bootstraps.put();
                self.state[c as usize] = State::Polling;
                self.seat[c as usize] = s;
                self.attempts[c as usize] = 0;
                self.authorized[c as usize] = 0;
                self.enrolled += 1;
                self.metrics.incr("ctl_enrollments");
                self.metrics
                    .observe("ctl_enroll_ns", now - self.started_ns[c as usize]);
                self.trace(now, 0xD1, u64::from(c), u64::from(s));
                // First time every client is enrolled (later re-enrollments
                // after crashes do not move it).
                if self.enrolled == self.live_clients
                    && self.metrics.get("ctl_all_enrolled_ns") == 0
                {
                    self.metrics.add("ctl_all_enrolled_ns", now.max(1));
                }
                self.poll(c, e, now);
            }
            Ev::Enrolled(_, _, s) => self.seats[s as usize].admission.bootstraps.put(),
            Ev::Poll(c, e) if self.live(c, e) => self.poll(c, e, now),
            Ev::Delivered(c, e, s, seq, mv, hash) => {
                self.release_write(s, now);
                if !self.live(c, e) {
                    return;
                }
                self.seats[s as usize].admission.end_poll(c, false);
                self.apply(c, seq, mv, hash, now);
                let delay = if self.slow[c as usize] {
                    self.settings.slow_delay_ns
                } else {
                    0
                };
                self.engine.after(delay, Ev::Poll(c, e));
            }
            Ev::Fault(i) => {
                self.fault(i as usize, now);
                self.sync(now);
            }
            Ev::Heal(i) => self.heal(i as usize),
            Ev::Restart(p) => {
                if !self.cp.alive(p) && self.state[p as usize] != State::Gone {
                    self.cp.restart(p);
                    self.metrics.incr("ctl_restarts");
                    if self.record {
                        self.changes.lives.push((p, Life::Restarted));
                    }
                }
            }
            Ev::Check => {
                self.check(now);
                self.engine.after(self.settings.check_ns, Ev::Check);
            }
            Ev::Enroll(..) | Ev::Poll(..) => {}
        }
    }

    fn live(&self, c: u32, e: u32) -> bool {
        self.epoch[c as usize] == e && self.state[c as usize] != State::Gone
    }

    fn enroll(&mut self, c: u32, e: u32, now: Nanos) {
        if self.partitioned[c as usize] {
            self.metrics.incr("ctl_unreachable");
            let d = self.settings.connect_timeout_ns;
            self.engine.after(d, Ev::Enroll(c, e));
            return;
        }
        let Some(s) = self.pick_seat(c, NO_SEAT) else {
            self.metrics.incr("ctl_no_ready_seat");
            let d = self.backoff(c);
            self.engine.after(d, Ev::Enroll(c, e));
            return;
        };
        self.state[c as usize] = State::Enrolling;
        if !self.seats[s as usize].admission.bootstraps.try_take() {
            self.metrics.incr("ctl_bootstrap_overloaded");
            let d = self.backoff(c);
            self.engine.after(d, Ev::Enroll(c, e));
            return;
        }
        let mut latency = self.settings.handshake_ns;
        let (needed, _) = self.settings.enrollment_calls();
        while self.authorized[c as usize] < needed {
            match self.authorize(c, s, now, &mut latency) {
                Ok(()) => self.authorized[c as usize] += 1,
                Err(()) => {
                    self.seats[s as usize].admission.bootstraps.put();
                    let d = self.backoff(c);
                    self.engine.after(d, Ev::Enroll(c, e));
                    return;
                }
            }
        }
        self.engine.after(latency, Ev::Enrolled(c, e, s));
    }

    /// One bearer authorization of client `c` on seat `s` (`auth.rs`
    /// `authorize`): TokenReview, Pod, DaemonSet and ServiceAccount, Node,
    /// in that order, through the seat's cache and budget under
    /// [`AuthModel::Cached`]. Adds the API latency to `latency`. Fails on
    /// a local budget refusal or an API 429.
    fn authorize(&mut self, c: u32, s: u8, now: Nanos, latency: &mut Nanos) -> Result<(), ()> {
        let cached = self.settings.auth == AuthModel::Cached;
        let key = client_key(c);
        let base = self.base;
        let at = |ns: Nanos| base + Duration::from_nanos(ns);
        let seat = s as usize;
        let mut reserved = 0;
        if cached
            && self.seats[seat]
                .auth
                .caller(&key, at(now + *latency))
                .is_some()
        {
            self.metrics.incr("ctl_auth_caller_hits");
            return Ok(());
        }
        if cached {
            let t = at(now + *latency);
            reserved = self.seats[seat].auth.calls(&key, t);
            match self.seats[seat]
                .budget
                .reserve(reserved, t, limits::AUTH_BUDGET_WAIT)
            {
                Some(wait) => {
                    if !wait.is_zero() {
                        self.metrics.incr("ctl_auth_waited");
                        self.metrics
                            .observe("ctl_auth_wait_ns", wait.as_nanos() as u64);
                    }
                    *latency += wait.as_nanos() as Nanos;
                }
                None => {
                    self.metrics.incr("ctl_auth_overloaded");
                    return Err(());
                }
            }
        }
        let pod = pod_path(c);
        let node = node_path(c);
        // (method, path, metric); None marks a cache check.
        let review_cached = cached && {
            let t = at(now + *latency);
            self.seats[seat].auth.review(&key, t).is_some()
        };
        let mut calls: Vec<(&str, &str, &[u8], &'static str)> = Vec::with_capacity(5);
        if !review_cached {
            calls.push(("POST", TOKEN_REVIEWS, b"{}", "ctl_api_tokenreviews"));
        }
        calls.push(("GET", &pod, b"", "ctl_api_gets"));
        let bindings_index = calls.len();
        calls.push(("GET", DAEMONSET, b"", "ctl_api_gets"));
        calls.push(("GET", SERVICE_ACCOUNT, b"", "ctl_api_gets"));
        calls.push(("GET", &node, b"", "ctl_api_gets"));
        let mut issued = 0u32;
        let mut throttled = false;
        let mut i = 0;
        while i < calls.len() {
            if i == bindings_index && cached {
                let t = at(now + *latency);
                if self.seats[seat].auth.bindings(t).is_some() {
                    i += 2;
                    continue;
                }
            }
            let (method, path, body, counter) = calls[i];
            let (status, _, l) = self.api.handle(method, path, body, now + *latency);
            issued += 1;
            if status == 429 {
                throttled = true;
                break;
            }
            self.metrics.incr(counter);
            *latency += l;
            if cached {
                let t = at(now + *latency);
                if method == "POST" {
                    self.seats[seat].auth.put_review(
                        key,
                        Reviewed::default(),
                        Duration::from_secs(3600),
                        t,
                    );
                } else if i == bindings_index + 1 {
                    self.seats[seat].auth.put_bindings(Bindings::default(), t);
                }
            }
            i += 1;
        }
        self.count_api(now, u64::from(issued));
        if throttled {
            self.metrics.incr("ctl_api_429");
            if cached {
                let t = at(now + *latency);
                let b = &mut self.seats[seat].budget;
                b.throttled(t);
                b.refund(reserved.saturating_sub(issued));
            }
            return Err(());
        }
        if cached {
            let t = at(now + *latency);
            self.seats[seat]
                .auth
                .put_caller(key, (), Duration::from_secs(3600), t);
        }
        Ok(())
    }

    fn poll(&mut self, c: u32, e: u32, now: Nanos) {
        if self.partitioned[c as usize] {
            self.metrics.incr("ctl_unreachable");
            let d = self.settings.connect_timeout_ns;
            self.engine.after(d, Ev::Poll(c, e));
            return;
        }
        let mut s = self.seat[c as usize];
        if s == NO_SEAT || self.seats[s as usize].served.is_none() {
            let Some(next) = self.pick_seat(c, s) else {
                self.metrics.incr("ctl_no_ready_seat");
                let d = self.backoff(c);
                self.engine.after(d, Ev::Poll(c, e));
                return;
            };
            if s != NO_SEAT {
                self.metrics.incr("ctl_seat_failovers");
            }
            s = next;
            self.seat[c as usize] = s;
        }
        let after = Some(self.applied_seq[c as usize]).filter(|a| *a > 0);
        let current = self.seats[s as usize].served;
        self.poll_ns[c as usize] = now;
        match self.seats[s as usize].admission.snapshot(c, after, current) {
            Ok(Poll::Reply) => self.reply(c, s, now),
            Ok(Poll::Wait) => self.seats[s as usize].waiters.push((c, e)),
            Err(ProtocolFailure::Unavailable) => {
                // The client is ahead of this seat (check_ahead): 503, try
                // another seat.
                self.metrics.incr("ctl_503_ahead");
                if let Some(next) = self.pick_seat(c, s) {
                    self.seat[c as usize] = next;
                }
                let d = self.backoff(c).min(SECONDS);
                self.engine.after(d, Ev::Poll(c, e));
            }
            Err(_) => {
                self.metrics.incr("ctl_poll_overloaded");
                let d = self.backoff(c);
                self.engine.after(d, Ev::Poll(c, e));
            }
        }
    }

    /// The poll deadline of client `c`'s current long poll plus the write
    /// queue grace (`server.rs` `queued_write_slot`).
    fn write_deadline(&self, c: u32, now: Nanos) -> Nanos {
        let wait = racer_control_wire::POLL_WAIT.as_nanos() as Nanos;
        let start = self.poll_ns[c as usize].min(now);
        let cycle = start + (now - start) / wait * wait;
        cycle + wait + limits::WRITE_QUEUE_GRACE.as_nanos() as Nanos
    }

    /// Answer a registered poll from seat `s` at its served sequence:
    /// take a write slot or queue for one.
    fn reply(&mut self, c: u32, s: u8, now: Nanos) {
        let e = self.epoch[c as usize];
        let deadline_ns = self.write_deadline(c, now);
        let seat = &mut self.seats[s as usize];
        if seat.served.is_none() {
            seat.admission.end_poll(c, false);
            self.engine.at(now, Ev::Poll(c, e));
            return;
        }
        let waiter = WriteWaiter {
            node: c,
            epoch: e,
            deadline_ns,
        };
        let mut expired = Vec::new();
        let (epoch, state) = (&self.epoch, &self.state);
        let admit = seat
            .admission
            .writes
            .admit(waiter, |w| expire(epoch, state, w, now, &mut expired));
        for x in expired {
            self.reject_write(x, s);
        }
        match admit {
            Admit::Take => self.write(c, s, now),
            Admit::Wait => self.metrics.incr("ctl_write_queued"),
            Admit::Full => self.reject_write(c, s),
        }
    }

    /// No write slot within the queue bound or deadline: 429 and retry.
    fn reject_write(&mut self, c: u32, s: u8) {
        let e = self.epoch[c as usize];
        self.seats[s as usize].admission.end_poll(c, false);
        self.metrics.incr("ctl_write_429");
        let d = self.backoff(c);
        self.engine.after(d, Ev::Poll(c, e));
    }

    /// A write on seat `s` finished: hand its slot to the oldest live
    /// queued poll. Queued polls whose deadline passed answer 429 (they
    /// learn it here, at the next release, an approximation of their own
    /// timer firing).
    fn release_write(&mut self, s: u8, now: Nanos) {
        let mut expired = Vec::new();
        let (epoch, state) = (&self.epoch, &self.state);
        let next = self.seats[s as usize]
            .admission
            .writes
            .release(|w| expire(epoch, state, w, now, &mut expired));
        for c in expired {
            self.reject_write(c, s);
        }
        if let Some(w) = next {
            self.metrics.incr("ctl_write_dequeued");
            self.write(w.node, s, now);
        }
    }

    /// Write the reply for `c` holding a write slot on seat `s`.
    fn write(&mut self, c: u32, s: u8, now: Nanos) {
        let e = self.epoch[c as usize];
        let Some(seq) = self.seats[s as usize].served else {
            self.release_write(s, now);
            self.seats[s as usize].admission.end_poll(c, false);
            self.engine.at(now, Ev::Poll(c, e));
            return;
        };
        let after = Some(self.applied_seq[c as usize]).filter(|a| *a > 0);
        let image = self.cp.image.as_ref().filter(|i| i.record.sequence == seq);
        let mv = match image {
            Some(i) => i.record.membership_version,
            None => self
                .commits
                .iter()
                .rev()
                .find(|(_, q, _)| *q == seq)
                .map_or(0, |(_, _, m)| *m),
        };
        let delta = image.and_then(|i| i.delta.as_ref());
        let base = delta.map(|(q, h, _)| (*q, h.as_str()));
        let fp = self.held[c as usize];
        let body = reply_body(base, after, fp);
        if body == Body::Full && base.is_some_and(|(q, _)| Some(q) == after) {
            self.metrics.incr("ctl_base_hash_mismatches");
        }
        let hash = image.map_or_else(
            || self.hashes.get(&seq).copied().unwrap_or(0),
            |i| fingerprint(&i.record.content_hash),
        );
        let (kind, bytes) = match body {
            Body::Delta => ("ctl_delta_replies", delta.map_or(0, |d| d.2.len())),
            Body::Full => (
                "ctl_full_replies",
                image.map_or(
                    self.cp
                        .record
                        .as_ref()
                        .map_or(0, |r| r.publication_bytes as usize),
                    |i| i.encoded.len(),
                ),
            ),
        };
        if kind == "ctl_full_replies" && after.is_some() {
            self.metrics.incr("ctl_full_fallbacks");
            // Why: the client skipped a version, holds the first publication
            // (the empty bootstrap membership, whose delta is no smaller than
            // the image), the image has no delta from the client's version,
            // or a hash mismatch.
            let why = if after.is_some_and(|a| seq > a + 1) {
                "ctl_full_fallbacks_skipped"
            } else if after == Some(1) && delta.is_none() {
                "ctl_full_fallbacks_first"
            } else if delta.is_none() {
                "ctl_full_fallbacks_no_delta"
            } else {
                "ctl_full_fallbacks_other"
            };
            self.metrics.incr(why);
        }
        self.metrics.incr(kind);
        self.metrics.add("ctl_reply_bytes", bytes as u64);
        self.metrics.incr("ctl_client_replies");
        let write = self.settings.write_ns
            + (bytes as u64).saturating_mul(SECONDS) / self.settings.slot_bps.max(1);
        self.engine
            .after(write, Ev::Delivered(c, e, s, seq, mv, hash));
    }

    fn apply(&mut self, c: u32, seq: u64, mv: u64, hash: u64, now: Nanos) {
        let (old_seq, old_mv) = (self.applied_seq[c as usize], self.applied_mv[c as usize]);
        self.invariants.check(
            "ctl_client_membership_monotonic",
            seq > old_seq && mv >= old_mv,
            now,
            Some(c),
            || format!("({old_seq}, {old_mv}) -> ({seq}, {mv})"),
        );
        if seq <= old_seq {
            return;
        }
        self.applied_seq[c as usize] = seq;
        self.applied_mv[c as usize] = mv;
        self.held[c as usize] = hash;
        if self.record && mv != old_mv {
            self.changes.applied.push((c, mv));
        }
        self.attempts[c as usize] = 0;
        if old_seq == 0 && self.cp.report_serving(c) {
            // First snapshot of this Pod: the dataplane opens its peer
            // listener, then reports (`Session::mark_serving`).
            self.metrics.incr("ctl_reports");
        }
        // Lag from the commit, or from the client's start if it joined
        // (or restarted) after the commit.
        if let Some(t) = self.cp.commit_at.get(&seq).copied() {
            let from = t.max(self.started_ns[c as usize]);
            self.metrics
                .observe("ctl_client_lag_ns", now.saturating_sub(from));
            if old_seq > 0 && mv != old_mv {
                // A client that already held a membership: pure propagation.
                self.metrics
                    .observe("ctl_member_lag_ns", now.saturating_sub(t));
            }
        }
        if mv != old_mv {
            // How long after the first client this one adopted `mv`: the
            // window in which a peer that adopted it asks one still behind.
            let first = *self.first_adopt.entry(mv).or_insert(now);
            if old_seq > 0 {
                self.metrics
                    .observe("ctl_adopt_spread_ns", now.saturating_sub(first));
            }
        }
        self.trace(now, 0xD2, u64::from(c), seq);
    }

    fn victims(&mut self, count: u32, seats_only: bool) -> Vec<u32> {
        let primary = self.cp.primary.holder;
        let mut out = Vec::new();
        if seats_only {
            for h in self.cp.seat_holders().into_iter().flatten() {
                if Some(h) != primary && self.cp.alive(h) && out.len() < count as usize {
                    out.push(h);
                }
            }
            return out;
        }
        let n = self.state.len() as u32;
        let mut tries = 0;
        while out.len() < count as usize && tries < 4 * count + 64 {
            tries += 1;
            let p = (self.rng.next_u64() % u64::from(n)) as u32;
            if Some(p) != primary && self.state[p as usize] != State::Gone && !out.contains(&p) {
                out.push(p);
            }
        }
        out
    }

    fn choose(&mut self, fraction: f64) -> Vec<u32> {
        let n = self.state.len() as u32;
        let threshold = (fraction.clamp(0.0, 1.0) * (1u64 << 53) as f64) as u64;
        (0..n)
            .filter(|_| self.rng.next_u64() >> 11 < threshold)
            .collect()
    }

    fn fault(&mut self, i: usize, now: Nanos) {
        self.metrics.incr("ctl_faults");
        self.trace(now, 0xF0, i as u64, 0);
        match self.faults[i].clone() {
            CtlFault::CrashPrimary {
                restart_after_ns, ..
            } => {
                if let Some(p) = self.cp.primary.holder.filter(|p| self.cp.alive(*p)) {
                    self.crash(p, restart_after_ns);
                }
            }
            CtlFault::CrashSeats {
                count,
                restart_after_ns,
                ..
            } => {
                for p in self.victims(count, true) {
                    self.crash(p, restart_after_ns);
                }
            }
            CtlFault::Partition {
                until_ns, fraction, ..
            } => {
                for c in self.choose(fraction) {
                    self.partitioned[c as usize] = true;
                }
                self.engine.at(until_ns, Ev::Heal(i as u32));
            }
            CtlFault::Slow {
                until_ns, fraction, ..
            } => {
                for c in self.choose(fraction) {
                    self.slow[c as usize] = true;
                }
                self.engine.at(until_ns, Ev::Heal(i as u32));
            }
            CtlFault::Grow { count, .. } => {
                let room = self.settings.pods.saturating_sub(self.state.len() as u32);
                for _ in 0..count.min(room) {
                    self.add_client();
                }
                self.metrics
                    .add("ctl_pods_added", u64::from(count.min(room)));
            }
            CtlFault::Shrink { count, .. } => {
                for p in self.victims(count, false) {
                    self.remove(p);
                }
            }
        }
    }

    fn heal(&mut self, i: usize) {
        match self.faults[i] {
            CtlFault::Partition { .. } => self.partitioned.iter_mut().for_each(|p| *p = false),
            CtlFault::Slow { .. } => self.slow.iter_mut().for_each(|s| *s = false),
            _ => {}
        }
    }

    /// Crash Pod `p`: its client process dies too (fresh state on restart).
    fn crash(&mut self, p: u32, restart_after_ns: Option<Nanos>) {
        self.cp.crash(p);
        self.metrics.incr("ctl_crashes");
        if self.record {
            self.changes.lives.push((p, Life::Crashed));
        }
        self.drop_client(p);
        self.state[p as usize] = State::Idle;
        if let Some(d) = restart_after_ns {
            self.engine.after(d, Ev::Restart(p));
            let e = self.epoch[p as usize];
            self.started_ns[p as usize] = self.engine.now() + d;
            self.live_clients += 1;
            self.engine.after(d, Ev::Enroll(p, e));
        }
    }

    fn remove(&mut self, p: u32) {
        self.cp.remove_pod(p);
        self.drop_client(p);
        if self.record {
            self.changes.lives.push((p, Life::Removed));
        }
        self.state[p as usize] = State::Gone;
        self.metrics.incr("ctl_pods_removed");
    }

    fn drop_client(&mut self, p: u32) {
        let c = p as usize;
        if self.state[c] == State::Polling {
            self.enrolled -= 1;
        }
        if self.state[c] != State::Gone {
            self.live_clients -= 1;
        }
        if self.seat[c] != NO_SEAT {
            self.seats[self.seat[c] as usize]
                .admission
                .end_poll(p, false);
        }
        self.epoch[c] += 1;
        self.seat[c] = NO_SEAT;
        self.attempts[c] = 0;
        self.authorized[c] = 0;
        // A restarted process starts with no applied snapshot.
        if self.record && self.applied_mv[c] != 0 {
            self.changes.applied.push((p, 0));
        }
        self.applied_seq[c] = 0;
        self.applied_mv[c] = 0;
        self.held[c] = 0;
    }

    /// Convergence: once faults have healed for `converge_ns`, every live
    /// client that started at least [`Self::bound`] ago must have applied
    /// every sequence committed `converge_ns` ago.
    fn check(&mut self, now: Nanos) {
        let current = self.commits.last().map_or(0, |c| c.1);
        let mut behind = Vec::new();
        for c in 0..self.state.len() {
            if self.state[c] == State::Polling {
                behind.push(current.saturating_sub(self.applied_seq[c]));
            }
        }
        for b in behind {
            self.metrics.observe("ctl_sequences_behind", b);
        }
        let bound = self.settings.converge_ns;
        let enrolling = self.bound();
        if now < self.settled_ns.saturating_add(bound) {
            return;
        }
        let required = self
            .commits
            .iter()
            .rev()
            .find(|(t, ..)| *t + bound <= now)
            .map(|c| c.1);
        let Some(required) = required else {
            return;
        };
        for c in 0..self.state.len() {
            if self.state[c] == State::Gone || !self.cp.alive(c as u32) {
                continue;
            }
            if self.started_ns[c] + enrolling > now {
                continue;
            }
            let applied = self.applied_seq[c];
            self.invariants.check(
                "ctl_client_converges",
                applied >= required,
                now,
                Some(c as u32),
                || format!("applied {applied} < required {required}"),
            );
        }
    }

    /// Convergence bound for a client since it started: `converge_ns` plus
    /// the time the API rate limit needs for every client's enrollment
    /// calls (an enrollment storm heals only when the limit lets everyone
    /// through). Healed faults use `converge_ns` alone.
    pub fn bound(&self) -> Nanos {
        let calls = f64::from(self.settings.pods) * self.settings.enrollment_calls().1 as f64;
        self.settings.converge_ns + (calls / self.settings.api_qps * 1e9) as Nanos
    }

    pub fn memory(&self) -> Vec<(&'static str, usize)> {
        let n = self.state.capacity();
        let clients = n * (1 + 4 + 1 + 1 + 1 + 8 + 8 + 1 + 1 + 8);
        let seats: usize = self
            .seats
            .iter()
            .map(|s| s.waiters.capacity() * 8 + s.admission.polls().0 * 16 + s.auth.len() * 200)
            .sum();
        vec![
            ("ctl_model", self.cp.approximate_bytes()),
            ("ctl_engine", self.engine.approximate_bytes()),
            ("ctl_clients", clients + seats),
            // Two small JSON objects per Pod in the fake API server.
            ("ctl_api_store", self.api.len() * 256),
            (
                "ctl_commits",
                self.commits.capacity() * 24 + self.hashes.capacity() * 24,
            ),
        ]
    }

    /// Final counters folded into the metrics.
    fn finish(&mut self) {
        let s = self.cp.stats.clone();
        let st = self.api.stats.clone();
        let m = &mut self.metrics;
        for (k, v) in [
            ("ctl_model_api_reads", s.api_reads),
            ("ctl_model_api_writes", s.api_writes),
            ("ctl_model_conflicts", s.conflicts),
            ("ctl_seat_claims", s.seat_claims),
            ("ctl_seat_releases", s.seat_releases),
            ("ctl_primary_terms", s.primary_terms),
            ("ctl_passes", s.passes),
            ("ctl_commits", s.commits),
            ("ctl_image_fetches", s.image_fetches),
            ("ctl_image_bytes", s.image_bytes),
            ("ctl_watch_events", s.watch_events),
            ("ctl_dual_primary", s.dual_primary),
            ("ctl_apiserver_requests", st.requests),
            ("ctl_apiserver_throttled", st.throttled),
        ] {
            let have = m.get(k);
            m.add(k, v.saturating_sub(have));
        }
    }

    pub fn report(&mut self, scenario: &str, seed: u64) -> Value {
        self.finish();
        let memory = self.memory();
        let total: usize = memory.iter().map(|(_, b)| b).sum();
        let pods = self.state.len().max(1);
        let enrolled = self.enrolled;
        json!({
            "scenario": scenario,
            "nodes": self.state.len(),
            "seed": seed,
            "duration_ns": self.now_ns(),
            "events": self.engine.processed(),
            "trace_hash": format!("{:016x}", self.trace_hash()),
            "metrics": self.metrics.to_json(),
            "control": {
                "live_clients": self.live_clients,
                "enrolled": enrolled,
                "sequence": self.cp.sequence(),
                "membership_version": self.committed_membership_version(),
                "primary": self.cp.primary.holder,
                "ready_seats": self.cp.ready_seats(),
                "model_stats": format!("{:?}", self.cp.stats),
            },
            "invariants": self.invariants.to_json(),
            "memory": {
                "components": memory.iter().map(|(k, v)| ((*k).to_owned(), json!(v))).collect::<serde_json::Map<_, _>>(),
                "accounted_bytes": total,
                "accounted_bytes_per_node": total / pods,
                "peak_rss_bytes": peak_rss_bytes(),
            },
        })
    }
}

// Paths as in cluster-control `kube::paths` (kube/mod.rs:590-614).
const NS: &str = "racer-system";
const TOKEN_REVIEWS: &str = "/apis/authentication.k8s.io/v1/tokenreviews";

fn pod_path(c: u32) -> String {
    format!("/api/v1/namespaces/{NS}/pods/p{c}")
}

fn node_path(c: u32) -> String {
    format!("/api/v1/nodes/n{c}")
}

const DAEMONSET: &str = "/apis/apps/v1/namespaces/racer-system/daemonsets/racer-dataplane";
const SERVICE_ACCOUNT: &str = "/api/v1/namespaces/racer-system/serviceaccounts/racer-dataplane";

/// Scenario defaults: duration and fault plan for `nodes` Pods.
pub fn plan(
    scenario: &str,
    nodes: u32,
    duration: Option<Nanos>,
) -> (Settings, Nanos, Vec<CtlFault>) {
    let mut s = Settings {
        pods: nodes,
        initial_pods: nodes,
        ..Settings::default()
    };
    let d = |default: Nanos| duration.unwrap_or(default);
    match scenario {
        "enrollment-storm" => {
            s.enroll_spread_ns = 5 * SECONDS;
            (s, d(600 * SECONDS), Vec::new())
        }
        "failover" => {
            let d = d(900 * SECONDS);
            let f = vec![
                CtlFault::CrashPrimary {
                    at_ns: d / 3,
                    restart_after_ns: Some(60 * SECONDS),
                },
                CtlFault::CrashSeats {
                    at_ns: d / 2,
                    count: 2,
                    restart_after_ns: Some(60 * SECONDS),
                },
                CtlFault::CrashPrimary {
                    at_ns: d / 2 + 5 * SECONDS,
                    restart_after_ns: None,
                },
            ];
            (s, d, f)
        }
        "ctl-churn" => {
            let d = d(1200 * SECONDS);
            s.initial_pods = (nodes / 16).max(1);
            let waves = 8u32;
            let per = (nodes - s.initial_pods).div_ceil(waves);
            let mut f: Vec<CtlFault> = (0..waves)
                .map(|w| CtlFault::Grow {
                    at_ns: d / 20 + u64::from(w) * (d / 2 - d / 20) / u64::from(waves),
                    count: per,
                })
                .collect();
            for w in 0..2u64 {
                f.push(CtlFault::Shrink {
                    at_ns: 3 * d / 5 + w * d / 20,
                    count: (nodes / 40).max(1),
                });
            }
            (s, d, f)
        }
        "lagging" => {
            let d = d(900 * SECONDS);
            let f = vec![
                CtlFault::Partition {
                    from_ns: d / 4,
                    until_ns: d / 4 + d / 6,
                    fraction: 0.1,
                },
                CtlFault::Slow {
                    from_ns: d / 4,
                    until_ns: d / 2,
                    fraction: 0.05,
                },
                // Membership changes while clients lag, so they fall behind.
                CtlFault::Shrink {
                    at_ns: d / 4 + d / 12,
                    count: (nodes / 100).max(1),
                },
                CtlFault::CrashSeats {
                    at_ns: d / 3,
                    count: 1,
                    restart_after_ns: Some(30 * SECONDS),
                },
            ];
            (s, d, f)
        }
        _ => (s, d(60 * SECONDS), Vec::new()),
    }
}

/// Run a control scenario. `overrides` is the `"control"` object from
/// `--config`, if any.
pub fn run(
    scenario: &str,
    nodes: u32,
    seed: u64,
    duration: Option<Nanos>,
    overrides: Option<&Value>,
) -> Result<Value, String> {
    let (mut settings, end, faults) = plan(scenario, nodes, duration);
    if let Some(o) = overrides {
        settings.apply_json(o)?;
        settings.initial_pods = settings.initial_pods.clamp(1, nodes);
    }
    let mut sim = ControlSim::new(settings, seed, faults);
    sim.advance_to(end);
    Ok(sim.report(scenario, seed))
}

// ---------------------------------------------------------------------------
// A compact, single-threaded model of the whole control plane, built from
// the real steps, for the simulator.
//
// [`ControlPlane`] holds `n` dataplane Pods (one per Node). Each runs the
// real [`Lottery`], [`SeatRenew`], [`Contend`], and [`Follower`] steps; the
// primary runs the real [`plan_pass`] and [`discovery`] over typed
// inventory, and commits through the real [`Writer`] fence. Leases and the
// state record are typed values with resourceVersion counters rather than
// JSON objects, and every API read or write is counted. Client long polls
// are modeled per seat with the real write-slot limit and the
// [`drain`] queue model, so the simulator sees 429s and delivery lag
// at 50k Pods without one socket or thread per Pod.
//
// Time is `u64` nanoseconds from an `Instant` base chosen by the caller;
// wall time is `wall_base + ns`. Randomness comes from one [`SplitMix64`]
// stream, so runs replay exactly.

/// Report annotation value of a modeled Pod (default shares, no rails).
const REPORT: &str = r#"{"shares":1,"rdma_nics":[]}"#;

/// Primary reconcile debounce, as in `primary.rs`.
const DEBOUNCE: Duration = Duration::from_secs(1);

/// Primary Lease write to watch event at the seat followers.
const WATCH_DELAY: Duration = Duration::from_millis(50);

/// Time to write one publication body to one client.
const WRITE_NS: u64 = 2_000_000;

/// Counters reported by the model.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Stats {
    pub api_reads: u64,
    pub api_writes: u64,
    pub conflicts: u64,
    pub seat_claims: u64,
    pub seat_releases: u64,
    pub primary_terms: u64,
    pub passes: u64,
    pub commits: u64,
    pub image_fetches: u64,
    pub image_bytes: u64,
    pub watch_events: u64,
    pub client_replies: u64,
    pub client_429s: u64,
    pub notify_registrations: u64,
    /// Longest time from a commit until every client was served.
    pub max_client_lag_ns: u64,
    /// Times two Pods both held a fresh primary term with a writable fence.
    pub dual_primary: u64,
}

#[derive(Clone, Debug, Default)]
pub(crate) struct Lease {
    rv: u64,
    uid: u64,
    spec: LeaseSpec,
    pub(crate) holder: Option<u32>,
    ready: Option<u64>,
    /// Last sequence this seat delivered to its clients.
    served: Option<u64>,
}

pub(crate) struct Pod {
    alive: bool,
    /// Bumped on crash so stale events are ignored.
    epoch: u32,
    lottery: Lottery,
    seat: Option<u8>,
    renew: Option<SeatRenew>,
    contend: Contend,
    follower: Follower,
    /// Model time of this Pod's next follower read; an `CpEv::Follow` at any
    /// other time was superseded by an earlier wake.
    follow_at: u64,
    /// Model time this Pod's follower last read the state.
    followed_at: Option<u64>,
    /// The follower's read in flight: the state as of its start.
    reading: Option<FollowRead>,
    /// A primary Lease change arrived during that read
    /// (`seat.rs` `follower` takes `primary_lease` before the read).
    woken_during_read: bool,
    lease_fresh_until: Option<u64>,
    primary: Option<Writer>,
}

impl Pod {
    /// A freshly started process (fresh lottery and fence) at `epoch`.
    fn new(epoch: u32) -> Self {
        Self {
            alive: true,
            epoch,
            lottery: Lottery::default(),
            seat: None,
            renew: None,
            contend: Contend::default(),
            follower: Follower::default(),
            follow_at: u64::MAX,
            followed_at: None,
            reading: None,
            woken_during_read: false,
            lease_fresh_until: None,
            primary: None,
        }
    }
}

/// Model time `p`'s follower read started (`now` if it never read).
fn p_followed(p: &Pod, now: u64) -> u64 {
    p.followed_at.unwrap_or(now)
}

/// What a follower's state Secret read returned (the image is fetched from
/// the primary after the read, as `seat::follow_once` does).
struct FollowRead {
    record: Option<StateRecord>,
    /// The follower's installation count at the read's start.
    installs: u64,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
enum CpEv {
    Lottery(u32),
    Attempt(u32, u8),
    Renew(u32),
    Contend(u32),
    Follow(u32),
    /// The follower's read answers.
    FollowDone(u32),
    PrimaryRenew(u32),
    Reconcile(u32),
    /// The node lifecycle controller marks a crashed Pod's Node not ready.
    NodeNotReady(u32),
    /// A watched Pod changed: one debounced primary pass, as `primary.rs`
    /// runs after any inventory change. Does not reschedule.
    Wake(u32),
}

/// The control plane of a simulated cluster.
pub struct ControlPlane {
    /// Bytes of one image copy from the primary to a seat (for accounting).
    pub(crate) image_bytes_per_member: u64,
    /// Time after a crash until the node lifecycle controller marks the
    /// Node `Ready=Unknown` (Kubernetes `node-monitor-grace-period`, 40 s
    /// by default). A crash here is the whole node: the kubelet stops
    /// heartbeating with it. `None` never marks crashed Nodes.
    pub(crate) node_monitor_grace: Option<Duration>,
    /// Pods carry the report annotation from creation, as dataplanes from
    /// before round 4 (join) did: they reported before enrollment. When
    /// false, a Pod reports only through [`ControlPlane::report_serving`],
    /// which the client model calls once the dataplane can serve peers.
    report_at_start: bool,
    /// Duration of a seat follower's state Secret read. The read answers
    /// the state as of its start, so a commit (and its primary Lease
    /// change) during the read is not in it (Known issue 48).
    pub(crate) follow_read_ns: u64,
    base: Instant,
    pub(crate) now: u64,
    seq: u64,
    queue: BinaryHeap<Reverse<(u64, u64, u32, CpEv)>>,
    rng: SplitMix64,
    pub(crate) pods: Vec<Pod>,
    seats: Vec<Lease>,
    pub(crate) primary: Lease,
    view: PrimaryView,
    view_changed_at: Option<Instant>,
    pub(crate) record: Option<StateRecord>,
    pub(crate) image: Option<Image>,
    pub(crate) commit_at: BTreeMap<u64, u64>,
    inventory: Inventory,
    ownership: Ownership,
    policy: Policy,
    cluster: String,
    /// Wall seconds of the last commit that changed the membership version
    /// in the current term (the join hold-down input).
    membership_changed: Option<u64>,
    /// A [`CpEv::Wake`] pass is queued.
    wake_pending: bool,
    pub stats: Stats,
}

fn uuid(i: u64, tag: u16) -> String {
    format!("{i:08x}-{tag:04x}-4000-8000-000000000000")
}

impl ControlPlane {
    /// Build a cluster of `pods` Nodes and Pods, all synced. Wall time
    /// starts at 1.8e9 Unix seconds.
    pub fn new(pods: u32, seed: u64, report_at_start: bool) -> Self {
        let ownership = Ownership {
            namespace: "racer-system".into(),
            daemonset: "racer-dataplane".into(),
            uid: uuid(0, 0xd5),
            service_account: "racer-dataplane".into(),
        };
        let mut inventory = Inventory {
            nodes_synced: true,
            pods_synced: true,
            caches_synced: true,
            ..Default::default()
        };
        for i in 0..pods {
            Self::add_objects(&mut inventory, &ownership, i, report_at_start);
        }
        let mut cp = Self {
            rng: SplitMix64::new(seed),
            image_bytes_per_member: 128,
            node_monitor_grace: Some(Duration::from_secs(40)),
            report_at_start,
            follow_read_ns: 20_000_000,
            base: Instant::now(),
            now: 0,
            seq: 0,
            queue: BinaryHeap::new(),
            pods: (0..pods).map(|_| Pod::new(0)).collect(),
            seats: (0..annotations::MAX_SEATS)
                .map(|i| Lease {
                    uid: 1000 + i as u64,
                    ..Default::default()
                })
                .collect(),
            primary: Lease {
                uid: 999,
                ..Default::default()
            },
            view: PrimaryView::default(),
            view_changed_at: None,
            record: None,
            image: None,
            commit_at: BTreeMap::new(),
            inventory,
            ownership,
            policy: Policy::from(racer_cluster_control::Rotation::default()),
            cluster: "c1c1c1c1-0000-4000-8000-000000000001".into(),
            membership_changed: None,
            wake_pending: false,
            stats: Stats::default(),
        };
        for i in 0..pods {
            cp.schedule(0, i, CpEv::Lottery(i));
        }
        cp
    }

    fn add_objects(inv: &mut Inventory, own: &Ownership, i: u32, report: bool) {
        let name = format!("n{i}");
        inv.nodes.insert(
            name.clone(),
            NodeInfo {
                name: name.clone(),
                uid: uuid(u64::from(i), 0),
                ..Default::default()
            },
        );
        inv.pods.insert(
            uuid(u64::from(i), 1),
            PodInfo {
                namespace: own.namespace.clone(),
                name: format!("p{i}"),
                uid: uuid(u64::from(i), 1),
                node_name: name,
                pod_ip: format!("10.{}.{}.{}", i >> 16, (i >> 8) & 255, i & 255),
                phase: "Running".into(),
                service_account: own.service_account.clone(),
                owner: Some(Owner {
                    api_version: "apps/v1".into(),
                    kind: "DaemonSet".into(),
                    name: own.daemonset.clone(),
                    uid: own.uid.clone(),
                }),
                annotations: if report {
                    BTreeMap::from([(annotations::REPORT.to_owned(), REPORT.to_owned())])
                } else {
                    BTreeMap::new()
                },
                ..Default::default()
            },
        );
    }

    /// The dataplane on `pod` opened its peer listener and patched its
    /// report annotation (one API write). The annotation outlives process
    /// restarts, like the real one. A new annotation wakes a primary pass
    /// through its Pod watch. Returns whether the annotation was new.
    pub fn report_serving(&mut self, pod: u32) -> bool {
        let Some(p) = self.inventory.pods.get_mut(&uuid(u64::from(pod), 1)) else {
            return false;
        };
        // Every process start patches once (`control.rs` `reporter`); an
        // unchanged value is no watch event.
        self.stats.api_writes += 1;
        if p.annotations.contains_key(annotations::REPORT) {
            return false;
        }
        p.annotations
            .insert(annotations::REPORT.to_owned(), REPORT.to_owned());
        self.stats.watch_events += 1;
        if let Some(primary) = self.primary.holder
            && !self.wake_pending
        {
            self.wake_pending = true;
            self.after(DEBOUNCE, primary, CpEv::Wake(primary));
        }
        true
    }

    /// Whether Pod `pod` carries its report annotation.
    pub fn reported(&self, pod: u32) -> bool {
        self.inventory
            .pods
            .get(&uuid(u64::from(pod), 1))
            .is_some_and(|p| p.annotations.contains_key(annotations::REPORT))
    }

    fn at(&self, ns: u64) -> Instant {
        self.base + Duration::from_nanos(ns)
    }

    fn wall(&self) -> SystemTime {
        SystemTime::UNIX_EPOCH + Duration::from_secs(1_800_000_000) + Duration::from_nanos(self.now)
    }

    fn schedule(&mut self, at: u64, pod: u32, ev: CpEv) {
        self.seq += 1;
        let epoch = self.pods.get(pod as usize).map_or(0, |p| p.epoch);
        self.queue.push(Reverse((at, self.seq, epoch, ev)));
    }

    fn after(&mut self, d: Duration, pod: u32, ev: CpEv) {
        let at = self.now + d.as_nanos() as u64;
        self.schedule(at, pod, ev);
    }

    /// Time of the next pending event.
    pub fn next_event_ns(&self) -> Option<u64> {
        self.queue.peek().map(|Reverse((t, ..))| *t)
    }

    /// Committed publication sequence.
    pub fn sequence(&self) -> Option<u64> {
        self.record.as_ref().map(|r| r.sequence)
    }

    /// Seat holders, by seat index.
    pub fn seat_holders(&self) -> Vec<Option<u32>> {
        self.seats.iter().map(|s| s.holder).collect()
    }

    /// Ready seats at the committed sequence.
    pub fn ready_seats(&self) -> usize {
        let seq = self.sequence();
        self.seats
            .iter()
            .filter(|s| s.holder.is_some() && s.ready.is_some() && s.ready == seq)
            .count()
    }

    /// Sequence seat `index` last served to its clients, while its holder
    /// is alive and still holds the seat.
    pub fn seat_served(&self, index: usize) -> Option<u64> {
        let s = self.seats.get(index)?;
        let h = s.holder?;
        let p = &self.pods[h as usize];
        (p.alive && p.seat == Some(index as u8)).then_some(s.served)?
    }

    /// Whether Pod `pod` is running.
    pub fn alive(&self, pod: u32) -> bool {
        self.pods.get(pod as usize).is_some_and(|p| p.alive)
    }

    /// Remove a Node and its Pod (cluster shrink): crash the Pod and delete
    /// its objects, so the next primary pass drops the member.
    pub fn remove_pod(&mut self, pod: u32) {
        if self.alive(pod) {
            self.crash(pod);
        }
        self.inventory.nodes.remove(&format!("n{pod}"));
        self.inventory.pods.remove(&uuid(u64::from(pod), 1));
    }

    /// Crash a Pod. Its Leases are left to expire. Its Node and Pod objects
    /// stay, as after a process restart; call [`ControlPlane::restart`] to
    /// bring it back with fresh process state.
    pub fn crash(&mut self, pod: u32) {
        let p = &mut self.pods[pod as usize];
        p.alive = false;
        p.epoch += 1;
        p.seat = None;
        p.renew = None;
        p.primary = None;
        if let Some(grace) = self.node_monitor_grace {
            self.after(grace, pod, CpEv::NodeNotReady(pod));
        }
    }

    /// Set Node `n{pod}`'s `Ready` condition, stamped now, if it changes.
    fn set_node_ready(&mut self, pod: u32, ready: bool) {
        let since = self
            .wall()
            .duration_since(SystemTime::UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs();
        if let Some(n) = self.inventory.nodes.get_mut(&format!("n{pod}"))
            && n.ready.is_none_or(|r| r.ready != ready)
        {
            n.ready = Some(Readiness {
                ready,
                since_unix: Some(since),
            });
        }
    }

    /// Whether Node `n{pod}` currently reports `Ready=True` (or nothing).
    pub fn node_ready(&self, pod: u32) -> bool {
        self.inventory
            .nodes
            .get(&format!("n{pod}"))
            .is_some_and(|n| n.ready.is_none_or(|r| r.ready))
    }

    /// Restart a crashed Pod with a new process (fresh lottery and fence).
    pub fn restart(&mut self, pod: u32) {
        let p = &mut self.pods[pod as usize];
        *p = Pod::new(p.epoch + 1);
        self.schedule(self.now, pod, CpEv::Lottery(pod));
        if !self.node_ready(pod) {
            self.set_node_ready(pod, true);
        }
    }

    /// Add a Node and its Pod (cluster growth).
    pub fn add_pod(&mut self) -> u32 {
        let i = self.pods.len() as u32;
        Self::add_objects(
            &mut self.inventory,
            &self.ownership,
            i,
            self.report_at_start,
        );
        self.pods.push(Pod::new(0));
        self.schedule(self.now, i, CpEv::Lottery(i));
        i
    }

    fn observe(&self, lease: &Lease, name: String) -> LeaseObservation {
        let mut a = BTreeMap::new();
        if let Some(h) = lease.holder {
            a.insert(annotations::HOLDER_NONCE.to_owned(), format!("{h}"));
            a.insert(annotations::HOLDER_NODE.to_owned(), format!("n{h}"));
            a.insert(annotations::HOLDER_UID.to_owned(), uuid(u64::from(h), 1));
            a.insert(annotations::CLUSTER_ID.to_owned(), self.cluster.clone());
            a.insert(
                annotations::ENDPOINT.to_owned(),
                format!("https://10.{}.{}.{}:8443", h >> 16, (h >> 8) & 255, h & 255),
            );
            a.insert(annotations::SERVING_CERTIFICATE.to_owned(), "AAAA".into());
            if let Some(r) = lease.ready {
                a.insert(annotations::READY.to_owned(), r.to_string());
            }
        }
        LeaseObservation {
            name,
            uid: format!("lease-{}", lease.uid),
            resource_version: lease.rv.to_string(),
            spec: lease.spec.clone(),
            annotations: a,
        }
    }

    fn identity(pod: u32) -> String {
        format!("p{pod}/{pod}")
    }

    /// Process every event up to and including `until_ns`.
    pub fn advance_to(&mut self, until_ns: u64) {
        while let Some(Reverse((t, _, epoch, ev))) = self.queue.peek().copied() {
            if t > until_ns {
                break;
            }
            self.queue.pop();
            self.now = t;
            let pod = match ev {
                CpEv::Lottery(p)
                | CpEv::Attempt(p, _)
                | CpEv::Renew(p)
                | CpEv::Contend(p)
                | CpEv::Follow(p)
                | CpEv::FollowDone(p)
                | CpEv::PrimaryRenew(p)
                | CpEv::Reconcile(p)
                | CpEv::NodeNotReady(p)
                | CpEv::Wake(p) => p,
            };
            if let CpEv::Wake(_) = ev {
                self.wake_pending = false;
            }
            let p = &self.pods[pod as usize];
            if p.epoch != epoch {
                continue;
            }
            if let CpEv::NodeNotReady(p) = ev {
                // Fires for a dead Pod; a restart bumped the epoch.
                self.set_node_ready(p, false);
                continue;
            }
            if !p.alive {
                continue;
            }
            match ev {
                CpEv::Lottery(p) => self.lottery(p),
                CpEv::Attempt(p, i) => self.attempt(p, i),
                CpEv::Renew(p) => self.renew(p),
                CpEv::Contend(p) => self.contend(p),
                CpEv::Follow(p) => self.follow(p),
                CpEv::FollowDone(p) => self.follow_done(p),
                CpEv::PrimaryRenew(p) => self.primary_renew(p),
                CpEv::Reconcile(p) => self.reconcile(p, true),
                CpEv::Wake(p) => self.reconcile(p, false),
                CpEv::NodeNotReady(_) => {}
            }
            self.check_primaries();
        }
        self.now = self.now.max(until_ns);
    }

    fn lottery(&mut self, pod: u32) {
        let now = self.at(self.now);
        let node = format!("n{pod}");
        let uid = uuid(u64::from(pod), 1);
        let p = &mut self.pods[pod as usize];
        let plan = p.lottery.plan(
            &rules::LotteryInputs {
                holds_seat: p.seat.is_some(),
                cluster_known: true,
                view: &self.view,
                primary_changed_at: self.view_changed_at,
                node: &node,
                pod_uid: &uid,
                initial_seats: limits::MIN_AUTO_SEATS,
            },
            now,
            &mut self.rng,
        );
        match plan {
            rules::Plan::Idle(d) | rules::Plan::NoVacancy(d) => {
                self.after(d, pod, CpEv::Lottery(pod))
            }
            rules::Plan::Attempt { delay, index } => {
                self.after(delay, pod, CpEv::Attempt(pod, index as u8))
            }
        }
    }

    fn attempt(&mut self, pod: u32, index: u8) {
        self.schedule(self.now, pod, CpEv::Lottery(pod));
        if self.pods[pod as usize].seat.is_some() {
            return;
        }
        let now = self.at(self.now);
        self.pods[pod as usize].lottery.begin();
        self.stats.api_reads += 1;
        let obs = self.observe(
            &self.seats[index as usize],
            annotations::seat_lease(index as usize),
        );
        if !self.pods[pod as usize].lottery.observe(
            index as usize,
            &obs,
            now,
            &format!("n{pod}"),
            &Self::identity(pod),
        ) {
            return;
        }
        // The claim PUT is CAS on the version just read; in this model reads
        // and writes of one event are atomic, so it succeeds.
        self.stats.api_writes += 1;
        let spec = election::claim(
            &obs.spec,
            &format!("p{pod}"),
            &election::now_micro(self.wall()),
        );
        let lease = &mut self.seats[index as usize];
        lease.rv += 1;
        lease.spec = spec;
        lease.holder = Some(pod);
        lease.ready = None;
        // A new holder serves nothing until its first follower read.
        lease.served = None;
        self.stats.seat_claims += 1;
        let p = &mut self.pods[pod as usize];
        p.lottery.won();
        p.seat = Some(index);
        p.renew = Some(SeatRenew::new(now));
        p.lease_fresh_until = Some(self.now + limits::LEASE_FRESHNESS.as_nanos() as u64);
        self.after(limits::LEASE_RENEW, pod, CpEv::Renew(pod));
        self.follow_at(pod, self.now);
        self.schedule(self.now, pod, CpEv::Contend(pod));
    }

    fn renew(&mut self, pod: u32) {
        let Some(index) = self.pods[pod as usize].seat else {
            return;
        };
        let now = self.at(self.now);
        let ready = self.pods[pod as usize]
            .follower
            .ready(now)
            .then(|| self.sequence())
            .flatten();
        let lease_ready = self.seats[index as usize].ready;
        let inputs = rules::RenewInputs::from_view(
            &self.view,
            index as usize,
            &format!("n{pod}"),
            self.pods[pod as usize].primary.is_some(),
            false,
            false,
            ready != lease_ready,
        );
        let Some(mut schedule) = self.pods[pod as usize].renew else {
            return;
        };
        let action = schedule.decide(now, inputs);
        match action {
            rules::Action::Wait => {}
            rules::Action::Release => {
                self.stats.api_writes += 1;
                self.stats.seat_releases += 1;
                let lease = &mut self.seats[index as usize];
                lease.rv += 1;
                lease.spec = election::release(&lease.spec);
                lease.holder = None;
                lease.ready = None;
                let p = &mut self.pods[pod as usize];
                p.seat = None;
                p.renew = None;
                p.primary = None;
                return;
            }
            rules::Action::Renew => {
                self.stats.api_writes += 1;
                schedule.put_started(now);
                let wall = election::now_micro(self.wall());
                let lease = &mut self.seats[index as usize];
                if lease.holder == Some(pod) {
                    lease.rv += 1;
                    lease.spec = election::renew(&lease.spec, &wall);
                    lease.ready = ready;
                    schedule.put_ok(now);
                    self.pods[pod as usize].lease_fresh_until =
                        Some(self.now + limits::LEASE_FRESHNESS.as_nanos() as u64);
                } else {
                    // Taken over after expiry: the re-read shows another term.
                    self.stats.conflicts += 1;
                    self.stats.api_reads += 1;
                    let _ = schedule.conflict(Some(false), now);
                    let p = &mut self.pods[pod as usize];
                    p.seat = None;
                    p.renew = None;
                    p.primary = None;
                    return;
                }
            }
        }
        self.pods[pod as usize].renew = Some(schedule);
        // Production also wakes on local changes; polling at the ready
        // interval stands in for that.
        let wait = schedule
            .next
            .saturating_duration_since(now)
            .clamp(Duration::from_millis(1), rules::READY_MIN_INTERVAL);
        self.after(wait, pod, CpEv::Renew(pod));
    }

    fn contend(&mut self, pod: u32) {
        if self.pods[pod as usize].seat.is_none() {
            return;
        }
        let now = self.at(self.now);
        let wait = Contend::wait(&mut self.rng);
        if self.pods[pod as usize].primary.is_some() {
            self.after(wait, pod, CpEv::Contend(pod));
            return;
        }
        self.stats.api_reads += 1;
        let obs = self.observe(&self.primary, annotations::PRIMARY_LEASE.into());
        if self.pods[pod as usize].contend.observe(&obs, now, true) {
            self.stats.api_writes += 1;
            self.primary.rv += 1;
            self.primary.spec = election::claim(
                &obs.spec,
                &format!("p{pod}"),
                &election::now_micro(self.wall()),
            );
            self.primary.holder = Some(pod);
            self.stats.primary_terms += 1;
            let term = Term {
                lease_uid: format!("lease-{}", self.primary.uid),
                transitions: self.primary.spec.transitions,
                holder: Self::identity(pod),
            };
            self.pods[pod as usize].primary = Some(Writer::new(term));
            // In-memory, per term, like `primary.rs`.
            self.membership_changed = None;
            self.pods[pod as usize].contend.term_ended();
            self.primary_changed();
            self.after(DEBOUNCE, pod, CpEv::Reconcile(pod));
            self.after(limits::LEASE_RENEW, pod, CpEv::PrimaryRenew(pod));
        }
        self.after(wait, pod, CpEv::Contend(pod));
    }

    fn primary_changed(&mut self) {
        self.view_changed_at = Some(self.at(self.now));
        self.view.holder = self.primary.holder.map(Self::identity);
        let alive = self.pods.iter().filter(|p| p.alive).count() as u64;
        self.stats.watch_events += alive;
        self.wake_followers();
    }

    fn primary_renew(&mut self, pod: u32) {
        if self.pods[pod as usize].primary.is_none() {
            return;
        }
        self.stats.api_writes += 1;
        if self.primary.holder != Some(pod) {
            self.stats.conflicts += 1;
            self.pods[pod as usize].primary = None;
            return;
        }
        self.primary.rv += 1;
        self.primary.spec = election::renew(&self.primary.spec, &election::now_micro(self.wall()));
        self.primary_changed();
        self.after(limits::LEASE_RENEW, pod, CpEv::PrimaryRenew(pod));
    }

    /// One primary pass. A `periodic` pass schedules the next one.
    fn reconcile(&mut self, pod: u32, periodic: bool) {
        if self.pods[pod as usize].primary.is_none() {
            return;
        }
        self.stats.passes += 1;
        // Seat reads, state read.
        self.stats.api_reads += annotations::MAX_SEATS as u64 + 2;
        let now = self.at(self.now);
        let current = self.record.clone();
        let lease_uid = format!("lease-{}", self.primary.uid);
        let writer = self.pods[pod as usize].primary.as_mut().expect("primary");
        if writer.observe(current.as_ref(), Some(&lease_uid)).is_err() {
            self.pods[pod as usize].primary = None;
            return;
        }
        let live: BTreeMap<usize, LeaseObservation> = self
            .seats
            .iter()
            .enumerate()
            .filter(|(_, s)| {
                s.holder.is_some_and(|h| {
                    self.pods[h as usize].alive && self.pods[h as usize].seat.is_some()
                })
            })
            .map(|(i, s)| (i, self.observe(s, annotations::seat_lease(i))))
            .collect();
        let writer = self.pods[pod as usize].primary.as_ref().expect("primary");
        let plan = plan_pass(
            &PassInputs {
                cluster: &self.cluster,
                pod_uid: &uuid(u64::from(pod), 1),
                seats: SeatPolicy::Auto,
                peer_port: 8082,
                policy: &self.policy,
                inventory: &self.inventory,
                synced: self.inventory.synced(),
                ownership: &self.ownership,
                current: current.as_ref(),
                previous_image: self.image.as_ref(),
                live: &live,
                writer,
                wall_unix: self
                    .wall()
                    .duration_since(SystemTime::UNIX_EPOCH)
                    .unwrap_or_default()
                    .as_secs(),
                membership_changed_unix: self.membership_changed,
            },
            &mut self.rng,
        );
        if let Ok(plan) = plan {
            let mut published = false;
            if plan.changed
                && current
                    .as_ref()
                    .is_none_or(|c| state::check_monotonic(c, &plan.next).is_ok())
            {
                self.stats.api_writes += 1;
                self.stats.commits += 1;
                if current
                    .as_ref()
                    .is_none_or(|c| c.membership_version != plan.next.membership_version)
                {
                    self.membership_changed = Some(
                        self.wall()
                            .duration_since(SystemTime::UNIX_EPOCH)
                            .unwrap_or_default()
                            .as_secs(),
                    );
                }
                if current
                    .as_ref()
                    .is_none_or(|c| c.sequence != plan.next.sequence)
                {
                    self.commit_at.insert(plan.next.sequence, self.now);
                    // The primary publishes the committed sequence on its
                    // Lease (`annotations::SEQUENCE`), which wakes the seat
                    // followers instead of leaving them to their next
                    // `FOLLOW_INTERVAL` read.
                    published = true;
                }
                self.record = Some(plan.next.clone());
            }
            if let Some(image) = plan.image
                && self
                    .record
                    .as_ref()
                    .is_some_and(|r| r.sequence == image.record.sequence)
            {
                self.image = Some(image);
            }
            let sequence = self.sequence();
            let d = discovery(&live, plan.desired, plan.eligible, sequence, None);
            let desired = d.extra[annotations::SEATS]
                .as_deref()
                .and_then(|v| v.parse().ok())
                .unwrap_or(plan.desired);
            let nodes: Vec<String> = d.extra[annotations::SEAT_NODES]
                .as_deref()
                .and_then(|v| serde_json::from_str(v).ok())
                .unwrap_or_default();
            let window = d.extra[annotations::CLAIM_WINDOW]
                .as_deref()
                .and_then(|v| v.parse().ok())
                .unwrap_or(30);
            if self.view.desired != desired
                || self.view.seat_nodes != nodes
                || self.view.claim_window != window
                || published
            {
                self.view.desired = desired;
                self.view.seat_nodes = nodes;
                self.view.claim_window = window;
                self.stats.api_writes += 1;
                self.primary.rv += 1;
                self.primary_changed();
            }
            // The primary serves its own image.
            if let Some(r) = self.record.clone() {
                let f = &mut self.pods[pod as usize].follower;
                let _ = f.install(r, now);
                if let Some(i) = &self.image {
                    f.install_image(i.record.clone());
                }
            }
        }
        if periodic {
            self.after(limits::LEASE_RENEW, pod, CpEv::Reconcile(pod));
        }
    }

    /// Schedule `pod`'s next follower read at `at`, superseding any other.
    fn follow_at(&mut self, pod: u32, at: u64) {
        self.pods[pod as usize].follow_at = at;
        self.schedule(at, pod, CpEv::Follow(pod));
    }

    /// The primary Lease changed: every Pod watches it, and a seat
    /// follower wakes on the change (`seat.rs` `follower` waits on
    /// `shared.primary_lease`, which `apply_primary` notifies), reading at
    /// most once a second. A follower whose read is in flight reads again
    /// once it completes (Known issue 48).
    fn wake_followers(&mut self) {
        let delay = WATCH_DELAY.as_nanos() as u64;
        let floor = Duration::from_secs(1).as_nanos() as u64;
        for i in 0..self.pods.len() {
            let p = &mut self.pods[i];
            if !p.alive || p.seat.is_none() {
                continue;
            }
            if p.reading.is_some() {
                p.woken_during_read = true;
                continue;
            }
            let at = (self.now + delay).max(p.followed_at.map_or(0, |t| t + floor));
            if at < p.follow_at {
                self.follow_at(i as u32, at);
            }
        }
    }

    fn follow(&mut self, pod: u32) {
        if self.pods[pod as usize].follow_at != self.now {
            // Superseded by an earlier wake.
            return;
        }
        self.pods[pod as usize].follow_at = u64::MAX;
        if self.pods[pod as usize].seat.is_none() {
            return;
        }
        if self.pods[pod as usize].reading.is_some() {
            // One read at a time (a seat claimed again while its last read
            // is in flight): that read's completion reads again.
            self.pods[pod as usize].woken_during_read = true;
            return;
        }
        self.stats.api_reads += 1;
        let read = FollowRead {
            record: self.record.clone(),
            installs: self.pods[pod as usize].follower.installs,
        };
        let p = &mut self.pods[pod as usize];
        p.followed_at = Some(self.now);
        p.reading = Some(read);
        p.woken_during_read = false;
        if self.follow_read_ns == 0 {
            return self.follow_done(pod);
        }
        let at = self.now + self.follow_read_ns;
        self.schedule(at, pod, CpEv::FollowDone(pod));
    }

    /// The follower's read answered: install what it returned, then wait
    /// for the next wake or `FOLLOW_INTERVAL`.
    fn follow_done(&mut self, pod: u32) {
        let p = &mut self.pods[pod as usize];
        let Some(read) = p.reading.take() else {
            return;
        };
        let woken = std::mem::take(&mut p.woken_during_read);
        let Some(index) = p.seat else {
            return;
        };
        let is_primary = p.primary.is_some();
        let now = self.at(self.now);
        // Confirmed as of the read's start, as `seat::follow_once` passes
        // `started`.
        let started = self.at(p_followed(&self.pods[pod as usize], self.now));
        let f = &self.pods[pod as usize].follower;
        let record = read
            .record
            .filter(|r| !rules::superseded(f.installs != read.installs, f.high_water.as_ref(), r));
        if let Some(r) = record {
            let f = &mut self.pods[pod as usize].follower;
            if f.install(r.clone(), started).is_ok()
                && rules::need_image(&r, f.image.as_ref(), is_primary)
                && let Some(i) = &self.image
                && i.record.sequence == r.sequence
            {
                self.stats.image_fetches += 1;
                self.stats.image_bytes +=
                    i.publication.members.len() as u64 * self.image_bytes_per_member;
                f.install_image(i.record.clone());
            }
            if f.ready(now) && self.seats[index as usize].served != Some(r.sequence) {
                self.seats[index as usize].served = Some(r.sequence);
                self.serve_clients(index, r.sequence);
            }
        }
        let next = if woken {
            let floor = Duration::from_secs(1).as_nanos() as u64;
            self.now
                .max(self.pods[pod as usize].followed_at.map_or(0, |t| t + floor))
        } else {
            self.now + limits::FOLLOW_INTERVAL.as_nanos() as u64
        };
        self.follow_at(pod, next);
    }

    /// A seat became ready at `sequence`: its share of client long polls
    /// receive the new image through the write slots.
    fn serve_clients(&mut self, _seat: u8, sequence: u64) {
        let seats = self
            .seats
            .iter()
            .filter(|s| s.holder.is_some())
            .count()
            .max(1) as u64;
        let alive = self.pods.iter().filter(|p| p.alive).count() as u64;
        let polls = alive.div_ceil(seats);
        // Woken polls queue for the write slots (`exec::Slots::acquire`);
        // only those still queued after the grace answer 429. A poll woken
        // mid-wait has at least the grace, so it is the model's bound.
        let (replies, last_ns) = drain(
            polls,
            limits::MAX_WRITES as u64,
            WRITE_NS,
            limits::WRITE_QUEUE_GRACE.as_nanos() as u64,
            limits::MAX_WRITE_WAITERS as u64,
        );
        self.stats.client_replies += replies;
        self.stats.client_429s += polls - replies;
        // Each woken poll re-registers its waker once (an O(1) slab insert;
        // before round 3 each scanned the list, W(W-1)/2 per notify).
        self.stats.notify_registrations += polls;
        if let Some(at) = self.commit_at.get(&sequence) {
            let lag = self.now.saturating_sub(*at) + last_ns;
            self.stats.max_client_lag_ns = self.stats.max_client_lag_ns.max(lag);
        }
    }

    fn check_primaries(&mut self) {
        let fresh = self
            .pods
            .iter()
            .filter(|p| {
                p.alive
                    && p.primary.as_ref().is_some_and(Writer::may_write)
                    && p.lease_fresh_until.is_some_and(|t| t > self.now)
            })
            .count();
        if fresh > 1 {
            self.stats.dual_primary += 1;
        }
    }

    /// Approximate heap bytes held by the model, for memory reports.
    pub fn approximate_bytes(&self) -> usize {
        let per_pod = std::mem::size_of::<Pod>() + 64;
        let inv = self.inventory.nodes.len() * 256 + self.inventory.pods.len() * 512;
        let image = self
            .image
            .as_ref()
            .map_or(0, |i| i.encoded.len() + i.publication.members.len() * 160);
        self.pods.len() * per_pod + inv + image + self.queue.len() * 40
    }
}

/// `polls` woken together, `write_slots` concurrent writes of `write_ns`
/// each, FIFO queue (`step::admission::WriteQueue`), each poll willing to
/// wait `wait_ns` for a slot and at most `max_waiting` queued. Round k
/// starts at `k * write_ns`. Returns the polls answered (the rest get 429)
/// and when the last reply finished, from the wake.
fn drain(
    polls: u64,
    write_slots: u64,
    write_ns: u64,
    wait_ns: u64,
    max_waiting: u64,
) -> (u64, u64) {
    let slots = write_slots.max(1);
    let overflow = polls.saturating_sub(slots).saturating_sub(max_waiting);
    // Rounds whose start is within the wait: 0..=wait_ns / write_ns.
    let rounds_in_time = wait_ns
        .checked_div(write_ns)
        .map_or(u64::MAX, |r| r.saturating_add(1));
    let replies = (polls - overflow).min(rounds_in_time.saturating_mul(slots));
    (replies, replies.div_ceil(slots).saturating_mul(write_ns))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn quick(scenario: &str, nodes: u32, secs: u64, seed: u64) -> Value {
        run(scenario, nodes, seed, Some(secs * SECONDS), None).unwrap()
    }

    fn counter(r: &Value, k: &str) -> u64 {
        r["metrics"]["counters"][k].as_u64().unwrap_or(0)
    }

    fn assert_clean(r: &Value) {
        assert_eq!(r["invariants"]["hard_failures"], 0, "{}", r["invariants"]);
        assert_eq!(counter(r, "ctl_dual_primary"), 0);
    }

    fn checks(r: &Value, name: &str) -> u64 {
        r["invariants"]["invariants"]
            .as_array()
            .unwrap()
            .iter()
            .find(|i| i["name"] == name)
            .map_or(0, |i| i["checks"].as_u64().unwrap())
    }

    #[test]
    fn enrollment_storm_enrolls_everyone() {
        let r = quick("enrollment-storm", 300, 300, 1);
        assert_clean(&r);
        assert_eq!(r["control"]["enrolled"], 300, "{}", r["control"]);
        assert!(counter(&r, "ctl_all_enrolled_ns") > 0);
        assert_eq!(
            counter(&r, "ctl_api_tokenreviews"),
            counter(&r, "ctl_enrollments")
        );
        assert!(counter(&r, "ctl_api_qps_peak") > 0);
        assert!(counter(&r, "ctl_client_replies") >= 300);
        assert!(
            r["metrics"]["histograms"]["ctl_client_lag_ns"]["count"]
                .as_u64()
                .unwrap()
                > 0
        );
    }

    #[test]
    fn storm_under_tight_api_limit_sees_429s() {
        let overrides = json!({"api_qps": 20.0, "api_burst": 20.0});
        let r = run(
            "enrollment-storm",
            200,
            2,
            Some(300 * SECONDS),
            Some(&overrides),
        )
        .unwrap();
        assert_clean(&r);
        assert!(
            counter(&r, "ctl_api_429") > 0,
            "{}",
            r["metrics"]["counters"]
        );
        assert_eq!(r["control"]["enrolled"], 200);
    }

    /// Known issue 11 (control-plane half): with the legacy authorization
    /// model an enrollment storm makes three full authorizations per
    /// client and the API server sheds them with 429s part way through.
    /// The cached model (`step::authz`) makes at most two, reuses the
    /// TokenReview and binding reads, and its adaptive budget keeps the
    /// seats' sum near the API rate, so 429s drop by an order of magnitude
    /// and everyone enrolls sooner.
    #[test]
    fn cached_authorization_cuts_api_calls_and_429s() {
        let storm = |auth: &str| {
            let overrides = json!({"api_qps": 300.0, "api_burst": 300.0, "auth": auth});
            let r = run(
                "enrollment-storm",
                3000,
                1,
                Some(400 * SECONDS),
                Some(&overrides),
            )
            .unwrap();
            assert_clean(&r);
            assert_eq!(r["control"]["enrolled"], 3000, "{auth}");
            r
        };
        let legacy = storm("legacy");
        let cached = storm("cached");
        let c = |r: &Value| r["metrics"]["counters"].clone();
        let calls = |r: &Value| counter(r, "ctl_api_tokenreviews") + counter(r, "ctl_api_gets");
        // Successful calls per enrollment: 15 before, about 3 after.
        assert!(calls(&legacy) >= 15 * 3000, "{}", c(&legacy));
        assert!(calls(&cached) <= 4 * 3000, "{}", c(&cached));
        assert!(counter(&cached, "ctl_auth_caller_hits") > 0);
        assert!(counter(&legacy, "ctl_api_tokenreviews") >= 3 * 3000);
        assert!(counter(&cached, "ctl_api_tokenreviews") < 2 * 3000);
        assert!(
            counter(&cached, "ctl_api_429") * 10 < counter(&legacy, "ctl_api_429"),
            "legacy {} cached {}",
            c(&legacy),
            c(&cached)
        );
        assert!(
            counter(&cached, "ctl_all_enrolled_ns") < counter(&legacy, "ctl_all_enrolled_ns"),
            "legacy {} cached {}",
            c(&legacy),
            c(&cached)
        );
    }

    /// The budget refuses locally, before any API call, when the seat is
    /// out of authorization budget; clients back off and still enroll.
    #[test]
    fn auth_budget_sheds_locally_and_everyone_enrolls() {
        let overrides = json!({"auth_qps": 20.0, "auth_burst": 10.0});
        let r = run(
            "enrollment-storm",
            500,
            1,
            Some(300 * SECONDS),
            Some(&overrides),
        )
        .unwrap();
        assert_clean(&r);
        assert_eq!(r["control"]["enrolled"], 500);
        assert!(
            counter(&r, "ctl_auth_overloaded") > 0,
            "{}",
            r["metrics"]["counters"]
        );
        assert_eq!(
            counter(&r, "ctl_api_429"),
            0,
            "{}",
            r["metrics"]["counters"]
        );
    }

    /// More polls per seat than write slots: woken polls queue and drain in
    /// FIFO order instead of answering 429 (Known issue 1).
    #[test]
    fn mass_wake_queues_for_write_slots() {
        let overrides = json!({"write_ns": 20_000_000u64});
        let r = run("ctl-churn", 600, 1, Some(600 * SECONDS), Some(&overrides)).unwrap();
        assert_clean(&r);
        let c = &r["metrics"]["counters"];
        assert!(counter(&r, "ctl_write_queued") > 0, "{c}");
        assert_eq!(
            counter(&r, "ctl_write_queued"),
            counter(&r, "ctl_write_dequeued") + counter(&r, "ctl_write_429"),
            "{c}"
        );
        assert_eq!(counter(&r, "ctl_write_429"), 0, "{c}");
        assert_eq!(
            counter(&r, "ctl_notify_registrations") > 0,
            counter(&r, "ctl_client_replies") > 0
        );
    }

    /// Writes slower than the queue wait allows: queued polls past their
    /// deadline still answer 429 (overload protection), and clients retry.
    #[test]
    fn slow_writes_still_shed_with_429() {
        let overrides = json!({"write_ns": 20_000_000_000u64});
        let r = run("ctl-churn", 600, 1, Some(300 * SECONDS), Some(&overrides)).unwrap();
        let c = &r["metrics"]["counters"];
        assert!(counter(&r, "ctl_write_429") > 0, "{c}");
        assert!(counter(&r, "ctl_write_dequeued") > 0, "{c}");
    }

    #[test]
    fn failover_elects_new_terms() {
        let r = quick("failover", 200, 900, 1);
        assert_clean(&r);
        assert!(
            counter(&r, "ctl_primary_terms") >= 3,
            "{}",
            r["metrics"]["counters"]
        );
        assert!(
            r["metrics"]["histograms"]["ctl_failover_ns"]["count"]
                .as_u64()
                .unwrap()
                >= 2
        );
        assert!(counter(&r, "ctl_crashes") >= 3);
        assert!(checks(&r, "ctl_client_converges") > 0);
    }

    #[test]
    fn churn_grows_and_shrinks() {
        let r = quick("ctl-churn", 320, 1200, 1);
        assert_clean(&r);
        assert_eq!(r["nodes"], 320);
        assert!(counter(&r, "ctl_pods_removed") > 0);
        assert!(
            counter(&r, "ctl_membership_versions") >= 8,
            "{}",
            r["metrics"]["counters"]
        );
        assert!(
            counter(&r, "ctl_delta_replies") > 0,
            "{}",
            r["metrics"]["counters"]
        );
        assert!(counter(&r, "ctl_full_replies") > 0);
        assert!(checks(&r, "ctl_client_converges") > 0);
        // A primary pass that changes nothing keeps the image's delta, so
        // a client one version behind never gets a full image for lack of
        // one (Known issue 20).
        assert_eq!(
            counter(&r, "ctl_full_fallbacks_no_delta"),
            0,
            "{}",
            r["metrics"]["counters"]
        );
    }

    #[test]
    fn lagging_clients_catch_up() {
        let r = quick("lagging", 300, 900, 1);
        assert_clean(&r);
        assert!(counter(&r, "ctl_unreachable") > 0);
        assert!(checks(&r, "ctl_client_converges") > 0);
        assert!(counter(&r, "ctl_full_fallbacks") > 0);
    }

    #[test]
    fn replays_exactly_and_seed_matters() {
        for s in SCENARIOS {
            let a = quick(s, 120, 200, 3);
            let b = quick(s, 120, 200, 3);
            assert_eq!(a["trace_hash"], b["trace_hash"], "{s}");
            assert_eq!(a["metrics"], b["metrics"], "{s}");
        }
        assert_ne!(
            quick("failover", 120, 200, 3)["trace_hash"],
            quick("failover", 120, 200, 4)["trace_hash"]
        );
    }

    #[test]
    fn membership_api_and_co_stepping() {
        let (settings, _, _) = plan("enrollment-storm", 60, None);
        let mut sim = ControlSim::new(settings, 9, Vec::new());
        assert_eq!(sim.membership_version(0), 0);
        assert_eq!(sim.membership_version(10_000), 0);
        let mut last = 0;
        // Co-step in small increments as a data-plane world would.
        while let Some(t) = sim.next_event_ns() {
            if t > 240 * SECONDS {
                break;
            }
            sim.advance_to(t);
            assert!(sim.now_ns() >= last);
            last = sim.now_ns();
        }
        let committed = sim.committed_membership_version().unwrap();
        let caught_up = (0..60)
            .filter(|n| sim.membership_version(*n) == committed)
            .count();
        assert!(caught_up > 50, "{caught_up}");
        assert!(sim.applied_seq[0] > 0);
        assert!(sim.cp.sequence().is_some());
    }

    /// Delta only for a client that applied the base sequence and holds its
    /// hash; restarted, lagging, and mismatched clients get the full image.
    #[test]
    fn delta_needs_the_held_base_hash() {
        let h = "00000000000000aa00000000000000000000000000000000000000000000beef";
        let other = "00000000000000bb00000000000000000000000000000000000000000000beef";
        let base = Some((7, h));
        assert_eq!(reply_body(base, Some(7), fingerprint(h)), Body::Delta);
        // Restarted: nothing applied, nothing held.
        assert_eq!(reply_body(base, None, 0), Body::Full);
        // Lagging two sequences behind.
        assert_eq!(reply_body(base, Some(5), fingerprint(h)), Body::Full);
        // At the base sequence with a different image.
        assert_eq!(reply_body(base, Some(7), fingerprint(other)), Body::Full);
        // At the base sequence but holding no hash.
        assert_eq!(reply_body(base, Some(7), 0), Body::Full);
        // No delta.
        assert_eq!(reply_body(None, Some(7), fingerprint(h)), Body::Full);
        assert_ne!(fingerprint("zz"), 0);
    }

    /// A client whose held hash differs from the delta base's gets full
    /// images until it applies one, then deltas again.
    #[test]
    fn mismatched_held_hash_falls_back_to_full() {
        let (settings, _, faults) = plan("ctl-churn", 120, None);
        let mut sim = ControlSim::new(settings, 2, faults);
        sim.advance_to(600 * SECONDS);
        for h in &mut sim.held {
            if *h != 0 {
                *h ^= 2;
            }
        }
        sim.advance_to(1200 * SECONDS);
        let r = sim.report("ctl-churn", 2);
        let c = |k: &str| r["metrics"]["counters"][k].as_u64().unwrap_or(0);
        assert!(
            c("ctl_base_hash_mismatches") > 0,
            "{}",
            r["metrics"]["counters"]
        );
        assert!(c("ctl_delta_replies") > 0);
        assert_eq!(r["invariants"]["hard_failures"], 0);
    }

    #[test]
    fn settings_overrides() {
        let mut s = Settings::default();
        s.apply_json(&json!({"api_qps": 5.0, "initial_pods": 3, "check_ns": 0}))
            .unwrap();
        assert_eq!((s.api_qps, s.initial_pods, s.check_ns), (5.0, 3, MILLIS));
        assert!(s.apply_json(&json!({"bogus": 1})).is_err());
        assert!(s.apply_json(&json!({"api_qps": -1})).is_err());
        assert!(s.apply_json(&json!(3)).is_err());
    }

    fn model(pods: u32, secs: u64, seed: u64) -> ControlPlane {
        let mut cp = ControlPlane::new(pods, seed, true);
        cp.advance_to(secs * 1_000_000_000);
        cp
    }

    #[test]
    fn cold_start_elects_seats_primary_and_publishes() {
        let cp = model(50, 240, 1);
        assert_eq!(cp.stats.primary_terms, 1, "{:?}", cp.stats);
        assert!(cp.primary.holder.is_some());
        assert_eq!(
            cp.seat_holders().iter().filter(|s| s.is_some()).count(),
            3,
            "{:?}",
            cp.seat_holders()
        );
        assert!(cp.sequence().is_some_and(|s| s >= 1));
        assert_eq!(cp.ready_seats(), 3, "{:?}", cp.stats);
        assert_eq!(cp.stats.dual_primary, 0);
        assert!(cp.stats.client_replies >= 50);
    }

    #[test]
    fn model_replays_exactly() {
        assert_eq!(model(30, 120, 7).stats, model(30, 120, 7).stats);
    }

    #[test]
    fn primary_failover_and_growth() {
        let mut cp = model(40, 240, 3);
        let first = cp.primary.holder.unwrap();
        let seq = cp.sequence().unwrap();
        cp.crash(first);
        let t = cp.now;
        cp.advance_to(t + 180_000_000_000);
        let second = cp.primary.holder.unwrap();
        assert_ne!(first, second);
        assert_eq!(cp.stats.primary_terms, 2);
        assert_eq!(cp.stats.dual_primary, 0);
        cp.add_pod();
        let t = cp.now;
        cp.advance_to(t + 60_000_000_000);
        assert!(cp.sequence().unwrap() > seq, "growth publishes");
        cp.restart(first);
        let t = cp.now;
        cp.advance_to(t + 60_000_000_000);
        assert!(cp.approximate_bytes() > 0);
    }

    fn members(cp: &ControlPlane) -> usize {
        cp.image.as_ref().map_or(0, |i| i.publication.members.len())
    }

    #[test]
    fn crashed_node_leaves_membership_after_grace_and_returns() {
        const S: u64 = 1_000_000_000;
        let mut cp = model(30, 240, 5);
        assert_eq!(members(&cp), 30);
        let mv = cp.record.as_ref().unwrap().membership_version;
        let victim = (0..30).find(|p| Some(*p) != cp.primary.holder).unwrap();
        cp.crash(victim);
        let t = cp.now;
        // Before the node monitor grace plus the readiness grace: kept.
        cp.advance_to(t + 39 * S);
        assert!(cp.node_ready(victim));
        cp.advance_to(t + 41 * S);
        assert!(!cp.node_ready(victim));
        cp.advance_to(t + 65 * S);
        assert_eq!(members(&cp), 30, "dropped inside the grace");
        assert_eq!(cp.record.as_ref().unwrap().membership_version, mv);
        // One pass period after both graces: gone, in one version.
        cp.advance_to(t + 85 * S);
        assert_eq!(members(&cp), 29);
        assert_eq!(cp.record.as_ref().unwrap().membership_version, mv + 1);
        cp.restart(victim);
        assert!(cp.node_ready(victim));
        let t = cp.now;
        cp.advance_to(t + 15 * S);
        assert_eq!(members(&cp), 30);
        assert_eq!(cp.record.as_ref().unwrap().membership_version, mv + 2);
    }

    /// Known issue 14: with reports only from serving dataplanes, a Pod
    /// joins after it reports, joins are batched by the hold, and seats
    /// are sized for every eligible Pod meanwhile.
    #[test]
    fn pods_join_once_serving_in_batches() {
        const S: u64 = 1_000_000_000;
        let mut cp = ControlPlane::new(40, 3, false);
        cp.advance_to(120 * S);
        assert_eq!(members(&cp), 0, "nobody serves yet");
        assert!(cp.sequence().is_some(), "an empty membership is published");
        assert_eq!(cp.ready_seats(), 3, "{:?}", cp.stats);
        assert!(!cp.reported(0));
        assert!(cp.report_serving(0));
        assert!(!cp.report_serving(0), "reported once");
        assert!(cp.reported(0));
        // Woken by the watch: the first member joins after the debounce.
        cp.advance_to(122 * S);
        assert_eq!(members(&cp), 1);
        let mv = cp.record.as_ref().unwrap().membership_version;
        for p in 1..40 {
            cp.report_serving(p);
            cp.advance_to(122 * S + u64::from(p) * S / 10);
        }
        // Inside the hold after the first join: nobody else yet.
        cp.advance_to(129 * S);
        assert_eq!(members(&cp), 1);
        cp.advance_to(160 * S);
        assert_eq!(members(&cp), 40);
        assert!(
            cp.record.as_ref().unwrap().membership_version <= mv + 2,
            "joins are batched"
        );
        // A crash and restart keeps the report: no new version.
        let mv = cp.record.as_ref().unwrap().membership_version;
        let victim = (0..40).find(|p| Some(*p) != cp.primary.holder).unwrap();
        cp.crash(victim);
        cp.advance_to(170 * S);
        cp.restart(victim);
        cp.advance_to(240 * S);
        assert!(cp.reported(victim));
        assert_eq!(cp.record.as_ref().unwrap().membership_version, mv);
    }

    /// Known issue 36: the primary publishes each commit on its Lease, so
    /// every seat serves it within the follower's one-second read floor, not
    /// at its next `FOLLOW_INTERVAL` read.
    #[test]
    fn seats_serve_a_commit_within_a_second() {
        const S: u64 = 1_000_000_000;
        let mut cp = model(30, 240, 5);
        let seq = cp.sequence().unwrap();
        let holders = cp.seat_holders();
        let victim = (0..30)
            .find(|p| Some(*p) != cp.primary.holder && !holders.contains(&Some(*p)))
            .unwrap();
        cp.crash(victim);
        // Step until the commit that drops the victim, then check every
        // seat 1.1 s later.
        let mut t = cp.now;
        while cp.sequence() == Some(seq) {
            t += S / 100;
            cp.advance_to(t);
            assert!(t < 400 * S, "no commit");
        }
        let committed = cp.commit_at[&cp.sequence().unwrap()];
        cp.advance_to(committed + S + S / 10);
        let seq = cp.sequence();
        let seats: Vec<usize> = (0..annotations::MAX_SEATS)
            .filter(|i| cp.seat_holders().get(*i).copied().flatten().is_some())
            .collect();
        assert!(!seats.is_empty());
        for i in seats {
            assert_eq!(cp.seat_served(i), seq, "seat {i}");
        }
    }

    /// Known issue 48: a commit that lands while a follower's read is in
    /// flight is served once that read completes plus at most one more read
    /// (the 1 s floor), not after another `FOLLOW_INTERVAL`. Slow reads make
    /// every commit overlap some follower's read.
    #[test]
    fn a_commit_during_a_follower_read_is_served_by_the_next_read() {
        const S: u64 = 1_000_000_000;
        let read = 2 * S;
        let mut cp = ControlPlane::new(30, 5, true);
        cp.follow_read_ns = read;
        cp.advance_to(240 * S);
        let mut overlapped = 0;
        for _ in 0..4 {
            let seq = cp.sequence().unwrap();
            let holders = cp.seat_holders();
            let victim = (0..30)
                .find(|p| {
                    Some(*p) != cp.primary.holder
                        && !holders.contains(&Some(*p))
                        && cp.node_ready(*p)
                })
                .unwrap();
            cp.crash(victim);
            let mut t = cp.now;
            while cp.sequence() == Some(seq) {
                t += S / 100;
                cp.advance_to(t);
                assert!(t < 1_000 * S, "no commit");
            }
            let committed = cp.commit_at[&cp.sequence().unwrap()];
            overlapped += cp
                .pods
                .iter()
                .filter(|p| p.alive && p.seat.is_some() && p.reading.is_some())
                .count();
            // A read in flight at the commit completes within `read`; the
            // next starts at the 1 s floor at the latest and takes `read`.
            cp.advance_to(committed + read.max(S) + read + S / 10);
            let seq = cp.sequence();
            for i in 0..annotations::MAX_SEATS {
                if cp.seat_holders().get(i).copied().flatten().is_some() {
                    let p = &cp.pods[cp.seat_holders()[i].unwrap() as usize];
                    assert_eq!(
                        cp.seat_served(i),
                        seq,
                        "seat {i}: commit at {committed}, read at {:?}",
                        p.followed_at
                    );
                }
            }
        }
        assert!(overlapped > 0, "no commit overlapped a read");
    }

    #[test]
    fn quick_restart_keeps_membership() {
        const S: u64 = 1_000_000_000;
        let mut cp = model(30, 240, 5);
        let mv = cp.record.as_ref().unwrap().membership_version;
        let victim = (0..30).find(|p| Some(*p) != cp.primary.holder).unwrap();
        cp.crash(victim);
        let t = cp.now;
        cp.advance_to(t + 30 * S);
        cp.restart(victim);
        cp.advance_to(t + 200 * S);
        assert!(cp.node_ready(victim));
        assert_eq!(members(&cp), 30);
        assert_eq!(cp.record.as_ref().unwrap().membership_version, mv);
    }

    #[test]
    fn accessors_and_removal() {
        let mut cp = model(30, 240, 5);
        let seq = cp.sequence().unwrap();
        assert_eq!(cp.record.as_ref().map(|r| r.sequence), Some(seq));
        assert_eq!(cp.image.as_ref().map(|i| i.record.sequence), Some(seq));
        assert!(cp.commit_at.get(&seq).copied().is_some_and(|t| t <= cp.now));
        let served: Vec<_> = (0..annotations::MAX_SEATS)
            .filter_map(|i| cp.seat_served(i))
            .collect();
        assert_eq!(served.len(), cp.ready_seats(), "{served:?}");
        assert_eq!(cp.pods.len() as u32, 30);
        let mv = cp.record.as_ref().unwrap().membership_version;
        let victim = (0..30).find(|p| Some(*p) != cp.primary.holder).unwrap();
        cp.remove_pod(victim);
        cp.remove_pod(victim);
        assert!(!cp.alive(victim));
        let t = cp.now;
        cp.advance_to(t + 60_000_000_000);
        assert!(cp.record.as_ref().unwrap().membership_version > mv);
        assert_eq!(cp.seat_served(99), None);
    }

    /// 50k Pods cold start. Run with `--ignored --release`.
    #[test]
    #[ignore]
    fn fifty_thousand_pods_cold_start() {
        let started = std::time::Instant::now();
        let cp = model(50_000, 300, 1);
        eprintln!(
            "50k: {:?} in {:?}, ~{} MiB model",
            cp.stats,
            started.elapsed(),
            cp.approximate_bytes() >> 20
        );
        assert_eq!(cp.stats.dual_primary, 0);
        assert!(cp.sequence().is_some());
        assert!(cp.ready_seats() >= 3);
        // Woken polls queue for write slots (round 3): no 429s.
        assert_eq!(cp.stats.client_429s, 0);
        assert_eq!(cp.stats.client_replies, cp.stats.notify_registrations);
    }

    /// More polls per seat than write slots: all are answered from the
    /// queue, none get 429 (before round 3 every round past the first 64
    /// answered 429).
    #[test]
    fn mass_wake_queues_instead_of_429() {
        let cp = model(600, 240, 3);
        assert!(cp.sequence().is_some());
        assert!(
            cp.stats.client_replies > 3 * limits::MAX_WRITES as u64,
            "{:?}",
            cp.stats
        );
        assert_eq!(cp.stats.client_429s, 0, "{:?}", cp.stats);
    }

    #[test]
    fn drain_queues_within_the_wait() {
        // 3125 polls, 64 slots, 2 ms writes, 5 s wait: 49 rounds, no 429.
        assert_eq!(
            drain(3125, 64, 2_000_000, 5_000_000_000, 16_384),
            (3125, 98_000_000)
        );
        // Wait shorter than the drain: rounds starting after it are 429.
        assert_eq!(drain(1000, 10, 1_000, 9_999, 16_384).0, 100);
        // Queue bound: beyond slots + max_waiting is 429 at once.
        assert_eq!(drain(100, 10, 1, u64::MAX, 20).0, 30);
        // Zero write time: everything admitted is answered.
        assert_eq!(drain(100, 10, 0, 0, 1000).0, 100);
        assert_eq!(drain(0, 64, 1, 1, 1), (0, 0));
        // 12,500 polls need 196 rounds of 64 slots; zero slots act as one.
        assert_eq!(drain(12_500, 64, 1, u64::MAX, u64::MAX).1, 196);
        assert_eq!(drain(5, 0, 1, u64::MAX, u64::MAX), (5, 5));
    }
}
