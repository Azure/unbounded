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
//!   ([`SeatAdmission::writes`], 64 per seat; none free is a 429), and
//!   [`body_for`] for delta versus full bodies. Waiters wake when their seat
//!   serves a new sequence; the wake costs [`cost::notify`] comparisons.
//! - Each client holds the content hash of the image it applied (none
//!   after a process restart) and sends it with its applied sequence, as
//!   production clients do; `body_for` answers a delta only when both
//!   match the image's delta base, else the full image.
//!
//! Faults: crash the primary or seat holders, restart, partition a fraction
//! of clients from the control plane, slow clients, grow and shrink.

use std::time::{Duration, Instant, SystemTime};

use racer_cluster_control::{
    annotations,
    apiserver::{Store, TokenBucket},
    limits,
    step::{
        admission::{Body, Poll, SeatAdmission, body_for},
        cluster::{ControlPlane, Settings as ModelSettings},
        cost,
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
        }
    }
}

impl Settings {
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
const API_CALLS_PER_ENROLLMENT: u64 = 5;

struct Seat {
    admission: SeatAdmission,
    served: Option<u64>,
    waiters: Vec<(u32, u32)>,
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

/// Changes since the last [`ControlSim::drain`], recorded only when
/// [`ControlSim::record_changes`] is on.
#[derive(Debug, Default)]
pub struct Changes {
    /// Newly committed membership versions with their member Pods.
    pub memberships: Vec<(u64, Vec<u32>)>,
    /// `(pod, membership version)` applied by its client (0 on restart).
    pub applied: Vec<(u32, u64)>,
    pub lives: Vec<(u32, Life)>,
}

impl Changes {
    pub fn is_empty(&self) -> bool {
        self.memberships.is_empty() && self.applied.is_empty() && self.lives.is_empty()
    }
}

/// Pod index of a published member: the model names Node `i` with the UID
/// `{i:08x}-0000-...` (`step::cluster` `uuid`).
fn member_pod(node: &str) -> Option<u32> {
    u32::from_str_radix(node.get(..8)?, 16).ok()
}

/// Control plane plus clients on one deterministic timeline.
pub struct ControlSim {
    pub settings: Settings,
    cp: ControlPlane,
    engine: Engine<Ev>,
    api: Store,
    seats: Vec<Seat>,
    rng: SplitMix64,
    // Clients, structure of arrays, indexed by Pod.
    state: Vec<State>,
    epoch: Vec<u32>,
    seat: Vec<u8>,
    attempts: Vec<u8>,
    applied_seq: Vec<u64>,
    applied_mv: Vec<u64>,
    /// Fingerprint of the content hash each client holds (0: none).
    held: Vec<u64>,
    partitioned: Vec<bool>,
    slow: Vec<bool>,
    started_ns: Vec<Nanos>,
    faults: Vec<CtlFault>,
    /// Time after which every live client must converge.
    settled_ns: Nanos,
    commits: Vec<(Nanos, u64, u64)>,
    /// Content hash fingerprint per committed sequence.
    hashes: std::collections::HashMap<u64, u64>,
    last_primary: Option<u32>,
    primary_lost_ns: Option<Nanos>,
    last_terms: u64,
    last_dual: u64,
    cp_api: (u64, u64),
    qps_second: u64,
    qps_count: u64,
    enrolled: u32,
    live_clients: u32,
    record: bool,
    changes: Changes,
    pub metrics: Metrics,
    pub invariants: Invariants,
}

impl ControlSim {
    pub fn new(settings: Settings, seed: u64, faults: Vec<CtlFault>) -> Self {
        let initial = settings.initial_pods.clamp(1, settings.pods.max(1));
        let cp = ControlPlane::new(
            ModelSettings {
                pods: initial,
                seed,
                image_bytes_per_member: settings.image_bytes_per_member,
                ..ModelSettings::default()
            },
            Instant::now(),
            SystemTime::UNIX_EPOCH + Duration::from_secs(1_800_000_000),
        );
        let mut api = Store::new();
        api.set_admission(Some(Box::new(TokenBucket::new(
            settings.api_qps,
            settings.api_burst,
            settings.api_latency_ns,
        ))));
        api.on_post(|path, _| {
            path.ends_with("/tokenreviews")
                .then(|| (201, json!({"status": {"authenticated": true}})))
        });
        api.put_object(
            &daemonset_path(),
            json!({"metadata": {"name": "racer-dataplane"}}),
        );
        api.put_object(
            &service_account_path(),
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
                })
                .collect(),
            state: Vec::new(),
            epoch: Vec::new(),
            seat: Vec::new(),
            attempts: Vec::new(),
            applied_seq: Vec::new(),
            applied_mv: Vec::new(),
            held: Vec::new(),
            partitioned: Vec::new(),
            slow: Vec::new(),
            started_ns: Vec::new(),
            settled_ns: faults.iter().map(CtlFault::heals_at).max().unwrap_or(0),
            faults,
            commits: Vec::new(),
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
        let p = self.cp.pod_count();
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
        self.applied_seq.push(0);
        self.applied_mv.push(0);
        self.held.push(0);
        self.partitioned.push(false);
        self.slow.push(false);
        self.started_ns.push(self.engine.now());
        self.live_clients += 1;
        let spread = self.settings.enroll_spread_ns.max(1);
        let delay = self.rng.next_u64() % spread;
        self.engine.after(delay, Ev::Enroll(c, 0));
        c
    }

    /// Record membership commits, applied versions and Pod lifecycle
    /// changes for [`Self::drain`].
    pub fn record_changes(&mut self) {
        self.record = true;
    }

    /// Take the changes recorded since the last call.
    pub fn drain(&mut self) -> Changes {
        std::mem::take(&mut self.changes)
    }

    /// Whether changes are waiting for [`Self::drain`].
    pub fn has_changes(&self) -> bool {
        !self.changes.is_empty()
    }

    /// Membership version the client on `node` has applied (0 before its
    /// first snapshot). Data-plane nodes use this to lag like their client.
    pub fn membership_version(&self, node: u32) -> u64 {
        self.applied_mv.get(node as usize).copied().unwrap_or(0)
    }

    /// Publication sequence the client on `node` has applied (0 if none).
    pub fn applied_sequence(&self, node: u32) -> u64 {
        self.applied_seq.get(node as usize).copied().unwrap_or(0)
    }

    /// The committed membership version, if any.
    pub fn committed_membership_version(&self) -> Option<u64> {
        self.cp.record().map(|r| r.membership_version)
    }

    /// The control plane model.
    pub fn model(&self) -> &ControlPlane {
        &self.cp
    }

    /// Current time.
    pub fn now_ns(&self) -> Nanos {
        self.engine.now().max(self.cp.now_ns())
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
            self.cp.primary_pod(),
            || format!("dual_primary {} -> {}", self.last_dual, s.dual_primary),
        );
        self.last_dual = s.dual_primary;
        // Primary availability: lost when its Pod dies, back on a new term.
        let primary = self.cp.primary_pod().filter(|p| self.cp.alive(*p));
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
        if let Some(r) = self.cp.record() {
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
                let commit = self.cp.commit_ns(seq).unwrap_or(at);
                if let Some((t, _, m)) = last {
                    self.metrics.observe("ctl_commit_interval_ns", commit - t);
                    if mv != m {
                        self.metrics.incr("ctl_membership_versions");
                    }
                } else {
                    self.metrics.incr("ctl_membership_versions");
                }
                if self.record && last.is_none_or(|(_, _, m)| m != mv) {
                    match self.cp.image().filter(|i| i.record.sequence == seq) {
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
                    .image()
                    .and_then(|i| i.delta.as_ref())
                    .filter(|_| self.cp.image().is_some_and(|i| i.record.sequence == seq))
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
            self.trace(at, 0xC4, i as u64, served.unwrap_or(u64::MAX));
            let waiters = std::mem::take(&mut self.seats[i].waiters);
            self.metrics.add(
                "ctl_notify_comparisons",
                cost::notify(waiters.len() as u64).comparisons,
            );
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
                    // reconnect (possibly to another seat).
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
                self.seats[s as usize].admission.writes.put();
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
        // auth.rs `authorize`: TokenReview, then Pod, DaemonSet,
        // ServiceAccount, and Node reads.
        let mut latency = self.settings.handshake_ns;
        let pod = pod_path(c);
        let node = node_path(c);
        let ds = daemonset_path();
        let sa = service_account_path();
        let calls: [(&str, &str, &[u8], &'static str); API_CALLS_PER_ENROLLMENT as usize] = [
            ("POST", TOKEN_REVIEWS, b"{}", "ctl_api_tokenreviews"),
            ("GET", &pod, b"", "ctl_api_gets"),
            ("GET", &ds, b"", "ctl_api_gets"),
            ("GET", &sa, b"", "ctl_api_gets"),
            ("GET", &node, b"", "ctl_api_gets"),
        ];
        let mut throttled = false;
        let mut issued = 0;
        for (method, path, body, counter) in calls {
            let (status, _, l) = self.api.handle(method, path, body, now + latency);
            issued += 1;
            if status == 429 {
                throttled = true;
                break;
            }
            self.metrics.incr(counter);
            latency += l;
        }
        debug_assert!(issued <= API_CALLS_PER_ENROLLMENT);
        self.count_api(now, issued);
        if throttled {
            self.metrics.incr("ctl_api_429");
            self.seats[s as usize].admission.bootstraps.put();
            let d = self.backoff(c);
            self.engine.after(d, Ev::Enroll(c, e));
            return;
        }
        self.engine.after(latency, Ev::Enrolled(c, e, s));
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

    /// Answer a registered poll from seat `s` at its served sequence.
    fn reply(&mut self, c: u32, s: u8, now: Nanos) {
        let e = self.epoch[c as usize];
        let seat = &mut self.seats[s as usize];
        let Some(seq) = seat.served else {
            seat.admission.end_poll(c, false);
            self.engine.at(now, Ev::Poll(c, e));
            return;
        };
        if !seat.admission.writes.try_take() {
            seat.admission.end_poll(c, false);
            self.metrics.incr("ctl_write_429");
            let d = self.backoff(c);
            self.engine.after(d, Ev::Poll(c, e));
            return;
        }
        let after = Some(self.applied_seq[c as usize]).filter(|a| *a > 0);
        let image = self.cp.image().filter(|i| i.record.sequence == seq);
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
                    self.cp.record().map_or(0, |r| r.publication_bytes as usize),
                    |i| i.encoded.len(),
                ),
            ),
        };
        if kind == "ctl_full_replies" && after.is_some() {
            self.metrics.incr("ctl_full_fallbacks");
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
        // Lag from the commit, or from the client's start if it joined
        // (or restarted) after the commit.
        if let Some(t) = self.cp.commit_ns(seq) {
            let from = t.max(self.started_ns[c as usize]);
            self.metrics
                .observe("ctl_client_lag_ns", now.saturating_sub(from));
        }
        self.trace(now, 0xD2, u64::from(c), seq);
    }

    fn victims(&mut self, count: u32, seats_only: bool) -> Vec<u32> {
        let primary = self.cp.primary_pod();
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
                if let Some(p) = self.cp.primary_pod().filter(|p| self.cp.alive(*p)) {
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
        let calls = f64::from(self.settings.pods) * API_CALLS_PER_ENROLLMENT as f64;
        self.settings.converge_ns + (calls / self.settings.api_qps * 1e9) as Nanos
    }

    pub fn memory(&self) -> Vec<(&'static str, usize)> {
        let n = self.state.capacity();
        let clients = n * (1 + 4 + 1 + 1 + 8 + 8 + 1 + 1 + 8);
        let seats: usize = self
            .seats
            .iter()
            .map(|s| s.waiters.capacity() * 8 + s.admission.polls().0 * 16)
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
                "primary": self.cp.primary_pod(),
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

fn daemonset_path() -> String {
    format!("/apis/apps/v1/namespaces/{NS}/daemonsets/racer-dataplane")
}

fn service_account_path() -> String {
    format!("/api/v1/namespaces/{NS}/serviceaccounts/racer-dataplane")
}

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
        assert!(sim.applied_sequence(0) > 0);
        assert!(sim.model().sequence().is_some());
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
}
