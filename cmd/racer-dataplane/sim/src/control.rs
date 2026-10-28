//! Control plane simulation: the real `racer_cluster_control` steps (seat
//! lottery and renewal, primary contention, primary passes, seat followers)
//! for every Pod, plus a client per Pod that enrolls and long-polls through
//! the real seat admission and authorization steps, all on one [`Engine`].
//!
//! Leases and the state record are typed values with resourceVersion
//! counters, not JSON objects; authorization calls go through a fake
//! [`Store`] with a [`TokenBucket`]. A co-stepped data plane reads
//! [`ControlSim::membership_version`] per node so its nodes lag behind the
//! committed membership exactly as their clients do.

use std::{
    collections::{BTreeMap, HashMap, HashSet},
    time::{Duration, Instant, SystemTime},
};

use racer_cluster_control::{
    PrimaryView, SeatPolicy, annotations,
    authority::{
        election::{self, LeaseObservation, LeaseSpec},
        keyring::Policy,
        members::{NodeInfo, Owner, Ownership, PodInfo, Readiness},
        publication::Image,
        state::{self, StateRecord, Term, Writer},
    },
    limits,
    step::{
        admission::{Admit, Body, Gate, Poll, WriteQueue, body_for, start_poll},
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

/// A queued write waiter still wants its slot at `now`. Connected waiters
/// whose deadline passed are collected so the caller can answer them 429.
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

/// The body for a client that applied `after` and holds the content hash
/// with fingerprint `held` (0: none), for an image whose delta base is
/// `base`. A different held hash is any other string to `body_for`.
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
    pub write_ns: Nanos,
    /// TLS and enrollment CPU on the seat.
    pub handshake_ns: Nanos,
    /// Client retry backoff: base and cap (doubling, with jitter).
    pub backoff_ns: Nanos,
    pub backoff_cap_ns: Nanos,
    pub connect_timeout_ns: Nanos,
    /// Extra delay before a slow client polls again after a reply.
    pub slow_delay_ns: Nanos,
    /// Invariant check period and convergence bound after faults heal.
    pub check_ns: Nanos,
    pub converge_ns: Nanos,
    /// Crash to Node `Ready=Unknown` (`node-monitor-grace-period`); 0 never
    /// marks crashed Nodes.
    pub node_monitor_grace_ns: Nanos,
    pub auth: AuthModel,
    /// Per-seat authorization API budget for [`AuthModel::Cached`].
    pub auth_qps: f64,
    pub auth_burst: f64,
    /// Pods report from creation, before enrolling, instead of when their
    /// client first applies a publication (as production does).
    pub report_at_start: bool,
    /// Duration of a seat follower's state read; it answers the state as of
    /// its start, so a commit during the read is not in it.
    pub follow_read_ns: Nanos,
}

/// How seats authorize an enrolling client's bearer token.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AuthModel {
    /// Three uncached authorizations of five API calls each, no budget.
    Legacy,
    /// Two authorizations through the seat's `AuthCache` and `ApiBudget`
    /// (`step::authz`).
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
            // 10 Gbit/s seat NIC shared by the write slots.
            slot_bps: 1_250_000_000 / limits::MAX_WRITES as u64,
            write_ns: 200_000,
            handshake_ns: 2 * MILLIS,
            backoff_ns: SECONDS,
            backoff_cap_ns: 30 * SECONDS,
            connect_timeout_ns: limits::IO_TIMEOUT.as_nanos() as u64,
            slow_delay_ns: 20 * SECONDS,
            check_ns: 10 * SECONDS,
            converge_ns: 180 * SECONDS,
            node_monitor_grace_ns: 40 * SECONDS,
            auth: AuthModel::Cached,
            auth_qps: limits::AUTH_API_QPS,
            auth_burst: limits::AUTH_API_BURST,
            report_at_start: false,
            follow_read_ns: 20 * MILLIS,
        }
    }
}

impl Settings {
    /// Authorizations per enrollment and the most API calls they make.
    fn enrollment_calls(&self) -> (u8, u64) {
        match self.auth {
            AuthModel::Legacy => (3, 15),
            // 5 for the first; the second hits the caller cache on the
            // same seat, or costs 5 more on another.
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
                "node_monitor_grace_ns" => self.node_monitor_grace_ns = u()?,
                "follow_read_ns" => self.follow_read_ns = u()?,
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
    Grow {
        at_ns: Nanos,
        count: u32,
    },
    /// Remove `count` live Pods (never the primary).
    Shrink {
        at_ns: Nanos,
        count: u32,
    },
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
    /// A control-plane step of Pod `.0`'s process at epoch `.1`.
    Pod(u32, u32, PodEv),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum PodEv {
    Lottery,
    Attempt(u8),
    Renew,
    Contend,
    Follow,
    /// The follower's read answers.
    FollowDone,
    PrimaryRenew,
    Reconcile,
    /// The node lifecycle controller marks a crashed Pod's Node not ready.
    NodeNotReady,
    /// A watched Pod changed: one debounced primary pass that does not
    /// reschedule.
    Wake,
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

/// Report annotation value of a modeled Pod.
const REPORT: &str = r#"{"shares":1,"rdma_nics":[]}"#;
/// Primary reconcile debounce, as in `primary.rs`.
const DEBOUNCE: Duration = Duration::from_secs(1);
/// Primary Lease write to watch event at the seat followers.
const WATCH_DELAY_NS: u64 = 50 * MILLIS;
/// A woken follower reads at most once a second.
const FOLLOW_FLOOR_NS: u64 = SECONDS;

#[derive(Clone, Debug, Default)]
struct Lease {
    rv: u64,
    uid: u64,
    spec: LeaseSpec,
    holder: Option<u32>,
    ready: Option<u64>,
}

/// A long poll queued for a write slot: the client, its connection epoch,
/// and the time after which it gives up.
#[derive(Clone, Copy, Debug)]
struct WriteWaiter {
    node: u32,
    epoch: u32,
    deadline_ns: u64,
}

/// One seat: its Lease and the seat server's admission state, as
/// `server::Server` keeps it (`Slots`, poll sets).
struct Seat {
    lease: Lease,
    /// Last sequence the holder's follower installed and served.
    lease_served: Option<u64>,
    writes: WriteQueue<WriteWaiter>,
    bootstraps: Gate,
    polls: HashSet<u32>,
    /// `lease_served` while the holder is alive and holds the seat, as the
    /// clients last saw it.
    served: Option<u64>,
    waiters: Vec<(u32, u32)>,
    /// The holder process's authorization cache and API budget.
    auth: AuthCache,
    budget: ApiBudget,
}

struct Pod {
    alive: bool,
    /// Bumped on crash and restart so stale events are ignored.
    epoch: u32,
    lottery: Lottery,
    seat: Option<u8>,
    renew: Option<SeatRenew>,
    contend: Contend,
    follower: Follower,
    /// Time of the next follower read; a `Follow` at any other time was
    /// superseded by an earlier wake.
    follow_at: u64,
    followed_at: Option<u64>,
    /// The follower's read in flight: the record as of its start and the
    /// follower's install count then.
    reading: Option<(Option<StateRecord>, u64)>,
    /// A primary Lease change arrived during that read.
    woken_during_read: bool,
    lease_fresh_until: Option<u64>,
    primary: Option<Writer>,
}

impl Pod {
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

/// Kubernetes API server rate limit: `qps` sustained, `burst` capacity.
struct ApiLimit {
    qps: f64,
    burst: f64,
    tokens: f64,
    at_ns: Nanos,
}

impl ApiLimit {
    /// Take a token at `now`; false answers 429.
    fn admit(&mut self, now: Nanos) -> bool {
        let elapsed = now.saturating_sub(self.at_ns) as f64 / 1e9;
        self.at_ns = self.at_ns.max(now);
        self.tokens = (self.tokens + elapsed * self.qps).min(self.burst);
        let ok = self.tokens >= 1.0;
        if ok {
            self.tokens -= 1.0;
        }
        ok
    }
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

/// Node `i` has UID `{i:08x}-{tag:04x}-...`; Pod `i` uses tag 1.
fn uuid(i: u64, tag: u16) -> String {
    format!("{i:08x}-{tag:04x}-4000-8000-000000000000")
}

/// Pod index of a published member (Node UID tag 0).
fn member_pod(node: &str) -> Option<u32> {
    u32::from_str_radix(node.get(..8)?, 16).ok()
}

fn identity(pod: u32) -> String {
    format!("p{pod}/{pod}")
}

/// Control plane plus clients on one deterministic timeline.
pub struct ControlSim {
    pub settings: Settings,
    engine: Engine<Ev>,
    api: ApiLimit,
    seats: Vec<Seat>,
    rng: SplitMix64,
    /// Origin of the simulated monotonic clock; wall time starts at 1.8e9
    /// Unix seconds.
    base: Instant,
    // Control plane.
    pods: Vec<Pod>,
    primary: Lease,
    view: PrimaryView,
    view_changed_at: Option<Instant>,
    committed: Option<StateRecord>,
    image: Option<Image>,
    commit_at: BTreeMap<u64, u64>,
    inventory: Inventory,
    ownership: Ownership,
    policy: Policy,
    cluster: String,
    /// Wall seconds of the last membership change in the current term.
    membership_changed: Option<u64>,
    wake_pending: bool,
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
    /// When each client's current long poll started (its deadline is a
    /// whole number of `POLL_WAIT`s later, as it re-polls on 204).
    poll_ns: Vec<Nanos>,
    faults: Vec<CtlFault>,
    /// Time after which every live client must converge.
    settled_ns: Nanos,
    commits: Vec<(Nanos, u64, u64)>,
    /// Content hash fingerprint per committed sequence.
    hashes: HashMap<u64, u64>,
    last_primary: Option<u32>,
    primary_lost_ns: Option<Nanos>,
    last_terms: u64,
    qps_second: u64,
    qps_count: u64,
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
        let mut sim = Self {
            rng: Streams::new(seed).stream(Role::Control, 0),
            engine: Engine::new(),
            api: ApiLimit {
                qps: settings.api_qps,
                burst: settings.api_burst,
                tokens: settings.api_burst,
                at_ns: 0,
            },
            seats: (0..annotations::MAX_SEATS)
                .map(|i| Seat {
                    lease: Lease {
                        uid: 1000 + i as u64,
                        ..Default::default()
                    },
                    lease_served: None,
                    writes: WriteQueue::new(limits::MAX_WRITES, limits::MAX_WRITE_WAITERS),
                    bootstraps: Gate::new(limits::MAX_BOOTSTRAPS),
                    polls: HashSet::new(),
                    served: None,
                    waiters: Vec::new(),
                    auth: AuthCache::default(),
                    budget: ApiBudget::new(settings.auth_qps, settings.auth_burst),
                })
                .collect(),
            base: Instant::now(),
            pods: Vec::new(),
            primary: Lease {
                uid: 999,
                ..Default::default()
            },
            view: PrimaryView::default(),
            view_changed_at: None,
            committed: None,
            image: None,
            commit_at: BTreeMap::new(),
            inventory: Inventory {
                nodes_synced: true,
                pods_synced: true,
                caches_synced: true,
                ..Default::default()
            },
            ownership: Ownership {
                namespace: "racer-system".into(),
                daemonset: "racer-dataplane".into(),
                uid: uuid(0, 0xd5),
                service_account: "racer-dataplane".into(),
            },
            policy: Policy::from(racer_cluster_control::Rotation::default()),
            cluster: "c1c1c1c1-0000-4000-8000-000000000001".into(),
            membership_changed: None,
            wake_pending: false,
            state: Vec::new(),
            epoch: Vec::new(),
            seat: Vec::new(),
            attempts: Vec::new(),
            authorized: Vec::new(),
            applied_seq: Vec::new(),
            applied_mv: Vec::new(),
            held: Vec::new(),
            partitioned: Vec::new(),
            slow: Vec::new(),
            started_ns: Vec::new(),
            poll_ns: Vec::new(),
            settled_ns: faults.iter().map(CtlFault::heals_at).max().unwrap_or(0),
            faults,
            commits: Vec::new(),
            hashes: HashMap::new(),
            last_primary: None,
            primary_lost_ns: None,
            last_terms: 0,
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

    /// Add a Node, its Pod and the Pod's client.
    fn add_client(&mut self) -> u32 {
        let c = self.state.len() as u32;
        let node = format!("n{c}");
        self.inventory.nodes.insert(
            node.clone(),
            NodeInfo {
                name: node.clone(),
                uid: uuid(u64::from(c), 0),
                ..Default::default()
            },
        );
        let own = &self.ownership;
        self.inventory.pods.insert(
            uuid(u64::from(c), 1),
            PodInfo {
                namespace: own.namespace.clone(),
                name: format!("p{c}"),
                uid: uuid(u64::from(c), 1),
                node_name: node,
                pod_ip: format!("10.{}.{}.{}", c >> 16, (c >> 8) & 255, c & 255),
                phase: "Running".into(),
                service_account: own.service_account.clone(),
                owner: Some(Owner {
                    api_version: "apps/v1".into(),
                    kind: "DaemonSet".into(),
                    name: own.daemonset.clone(),
                    uid: own.uid.clone(),
                }),
                annotations: if self.settings.report_at_start {
                    BTreeMap::from([(annotations::REPORT.to_owned(), REPORT.to_owned())])
                } else {
                    BTreeMap::new()
                },
                ..Default::default()
            },
        );
        self.pods.push(Pod::new(0));
        self.schedule(self.engine.now(), c, PodEv::Lottery);
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
    /// first snapshot).
    pub fn membership_version(&self, node: u32) -> u64 {
        self.applied_mv.get(node as usize).copied().unwrap_or(0)
    }

    pub fn committed_membership_version(&self) -> Option<u64> {
        self.committed.as_ref().map(|r| r.membership_version)
    }

    pub fn now_ns(&self) -> Nanos {
        self.engine.now()
    }

    pub fn next_event_ns(&self) -> Option<Nanos> {
        self.engine.next_at()
    }

    pub fn trace_hash(&self) -> u64 {
        self.engine.trace.value()
    }

    /// Run every event up to and including `until`.
    pub fn advance_to(&mut self, until: Nanos) {
        while let Some(ev) = self.engine.pop_until(until) {
            self.handle(ev);
        }
        self.engine.finish(until);
    }

    fn trace(&mut self, at: Nanos, tag: u64, a: u64, b: u64) {
        self.engine.trace.record(at, tag, a, b);
    }

    /// Count `n` API calls toward the per-second peak.
    fn api_calls(&mut self, n: u64) {
        let second = self.engine.now() / SECONDS;
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

    /// Fold control-plane changes into the client model after a step.
    fn sync(&mut self, at: Nanos) {
        let primary = self.primary.holder.filter(|p| self.alive(*p));
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
        for i in 0..self.seats.len() {
            let served = self.seat_served(i);
            if served == self.seats[i].served {
                continue;
            }
            let previous = self.seats[i].served;
            self.seats[i].served = served;
            self.trace(at, 0xC4, i as u64, served.unwrap_or(u64::MAX));
            let waiters = std::mem::take(&mut self.seats[i].waiters);
            // One O(1) waker registration per waiter (`exec::Changed`).
            self.metrics
                .add("ctl_notify_registrations", waiters.len() as u64);
            if served.is_some() && previous.is_some() {
                for (c, e) in waiters {
                    if self.epoch[c as usize] == e && self.state[c as usize] == State::Polling {
                        self.reply(c, i as u8, at);
                    }
                }
                continue;
            }
            // Seat lost or new holder: connections and queued polls drop,
            // clients reconnect; a new holder process has a cold cache.
            let seat = &mut self.seats[i];
            seat.auth = AuthCache::default();
            seat.budget = ApiBudget::new(self.settings.auth_qps, self.settings.auth_burst);
            let queued = seat.writes.take_waiting();
            let waiters = waiters
                .into_iter()
                .chain(queued.into_iter().map(|w| (w.node, w.epoch)));
            for (c, e) in waiters {
                self.seats[i].polls.remove(&c);
                if self.epoch[c as usize] == e {
                    self.engine.at(at, Ev::Poll(c, e));
                }
            }
        }
    }

    /// A new commit of `r` at `at`: client-model bookkeeping.
    fn committed_at(&mut self, at: Nanos) {
        let Some(r) = self.committed.as_ref() else {
            return;
        };
        let (seq, mv) = (r.sequence, r.membership_version);
        self.hashes.insert(seq, fingerprint(&r.content_hash));
        let last = self.commits.last().copied();
        let ok = last.is_none_or(|(_, s, m)| seq > s && mv >= m);
        self.invariants
            .check("ctl_sequence_monotonic", ok, at, None, || {
                format!("{last:?} -> ({seq}, {mv})")
            });
        if last.is_none_or(|(_, _, m)| m != mv) {
            self.metrics.incr("ctl_membership_versions");
            if self.record
                && let Some(i) = self.image.as_ref().filter(|i| i.record.sequence == seq)
            {
                let pods = i
                    .publication
                    .members
                    .iter()
                    .filter_map(|m| member_pod(&m.node.0))
                    .collect();
                self.changes.memberships.push((mv, pods));
            }
        }
        self.commits.push((at, seq, mv));
        self.trace(at, 0xC3, seq, mv);
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
            Ev::Pod(p, epoch, ev) => {
                self.pod_step(p, epoch, ev);
                self.sync(now);
            }
            Ev::Enroll(c, e) if self.live(c, e) => self.enroll(c, e, now),
            Ev::Enrolled(c, e, s) if self.live(c, e) => {
                self.seats[s as usize].bootstraps.put();
                self.state[c as usize] = State::Polling;
                self.seat[c as usize] = s;
                self.attempts[c as usize] = 0;
                self.authorized[c as usize] = 0;
                self.enrolled += 1;
                self.metrics.incr("ctl_enrollments");
                self.metrics
                    .observe("ctl_enroll_ns", now - self.started_ns[c as usize]);
                self.trace(now, 0xD1, u64::from(c), u64::from(s));
                if self.enrolled == self.live_clients
                    && self.metrics.get("ctl_all_enrolled_ns") == 0
                {
                    self.metrics.add("ctl_all_enrolled_ns", now.max(1));
                }
                self.poll(c, e, now);
            }
            Ev::Enrolled(_, _, s) => self.seats[s as usize].bootstraps.put(),
            Ev::Poll(c, e) if self.live(c, e) => self.poll(c, e, now),
            Ev::Delivered(c, e, s, seq, mv, hash) => {
                self.release_write(s, now);
                if !self.live(c, e) {
                    return;
                }
                self.seats[s as usize].polls.remove(&c);
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
            Ev::Heal(i) => match self.faults[i as usize] {
                CtlFault::Partition { .. } => self.partitioned.fill(false),
                CtlFault::Slow { .. } => self.slow.fill(false),
                _ => {}
            },
            Ev::Restart(p) => {
                if !self.alive(p) && self.state[p as usize] != State::Gone {
                    let pod = &mut self.pods[p as usize];
                    *pod = Pod::new(pod.epoch + 1);
                    self.schedule(now, p, PodEv::Lottery);
                    self.set_node_ready(p, true);
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
            let d = self.backoff(c);
            self.engine.after(d, Ev::Enroll(c, e));
            return;
        };
        self.state[c as usize] = State::Enrolling;
        if !self.seats[s as usize].bootstraps.try_take() {
            let d = self.backoff(c);
            self.engine.after(d, Ev::Enroll(c, e));
            return;
        }
        let mut latency = self.settings.handshake_ns;
        let (needed, _) = self.settings.enrollment_calls();
        while self.authorized[c as usize] < needed {
            if self.authorize(c, s, now, &mut latency) {
                self.authorized[c as usize] += 1;
            } else {
                self.seats[s as usize].bootstraps.put();
                let d = self.backoff(c);
                self.engine.after(d, Ev::Enroll(c, e));
                return;
            }
        }
        self.engine.after(latency, Ev::Enrolled(c, e, s));
    }

    /// One bearer authorization of client `c` on seat `s` (`auth.rs`
    /// `authorize`): TokenReview, Pod, DaemonSet and ServiceAccount, Node,
    /// through the seat's cache and budget under [`AuthModel::Cached`].
    /// Adds the API latency to `latency`. False on a local budget refusal
    /// or an API 429.
    fn authorize(&mut self, c: u32, s: u8, now: Nanos, latency: &mut Nanos) -> bool {
        let cached = self.settings.auth == AuthModel::Cached;
        let key = client_key(c);
        let base = self.base;
        let at = |ns: Nanos| base + Duration::from_nanos(ns);
        let seat = &mut self.seats[s as usize];
        let mut reserved = 0;
        if cached {
            let t = at(now + *latency);
            if seat.auth.caller(&key, t).is_some() {
                self.metrics.incr("ctl_auth_caller_hits");
                return true;
            }
            reserved = seat.auth.calls(&key, t);
            let Some(wait) = seat.budget.reserve(reserved, t, limits::AUTH_BUDGET_WAIT) else {
                self.metrics.incr("ctl_auth_overloaded");
                return false;
            };
            *latency += wait.as_nanos() as Nanos;
        }
        let (api, metrics) = (&mut self.api, &mut self.metrics);
        let api_latency = self.settings.api_latency_ns;
        let mut issued = 0u32;
        let mut call = |counter: &'static str, latency: &mut Nanos| {
            issued += 1;
            let ok = api.admit(now + *latency);
            if ok {
                metrics.incr(counter);
                *latency += api_latency;
            }
            ok
        };
        let mut ok = cached && seat.auth.review(&key, at(now + *latency)).is_some() || {
            let ok = call("ctl_api_tokenreviews", latency);
            if ok && cached {
                let t = at(now + *latency);
                seat.auth
                    .put_review(key, Reviewed::default(), Duration::from_secs(3600), t);
            }
            ok
        };
        ok = ok && call("ctl_api_gets", latency);
        if ok && !(cached && seat.auth.bindings(at(now + *latency)).is_some()) {
            ok = call("ctl_api_gets", latency) && call("ctl_api_gets", latency);
            if ok && cached {
                seat.auth
                    .put_bindings(Bindings::default(), at(now + *latency));
            }
        }
        ok = ok && call("ctl_api_gets", latency);
        if !ok {
            if cached {
                seat.budget.throttled(at(now + *latency));
                seat.budget.refund(reserved.saturating_sub(issued));
            }
            self.metrics.incr("ctl_api_429");
        } else if cached {
            seat.auth
                .put_caller(key, (), Duration::from_secs(3600), at(now + *latency));
        }
        self.metrics
            .add("ctl_apiserver_requests", u64::from(issued));
        self.api_calls(u64::from(issued));
        ok
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
                let d = self.backoff(c);
                self.engine.after(d, Ev::Poll(c, e));
                return;
            };
            s = next;
            self.seat[c as usize] = s;
        }
        let after = Some(self.applied_seq[c as usize]).filter(|a| *a > 0);
        let current = self.seats[s as usize].served;
        self.poll_ns[c as usize] = now;
        match start_poll(&mut self.seats[s as usize].polls, &c, after, current) {
            Ok(Poll::Reply) => self.reply(c, s, now),
            Ok(Poll::Wait) => self.seats[s as usize].waiters.push((c, e)),
            Err(ProtocolFailure::Unavailable) => {
                // Ahead of this seat (`check_ahead`): 503, try another.
                if let Some(next) = self.pick_seat(c, s) {
                    self.seat[c as usize] = next;
                }
                let d = self.backoff(c).min(SECONDS);
                self.engine.after(d, Ev::Poll(c, e));
            }
            Err(_) => {
                let d = self.backoff(c);
                self.engine.after(d, Ev::Poll(c, e));
            }
        }
    }

    /// Answer a registered poll from seat `s` at its served sequence: take
    /// a write slot or queue for one until the poll deadline plus the
    /// queue grace (`server.rs` `queued_write_slot`).
    fn reply(&mut self, c: u32, s: u8, now: Nanos) {
        let e = self.epoch[c as usize];
        let wait = racer_control_wire::POLL_WAIT.as_nanos() as Nanos;
        let start = self.poll_ns[c as usize].min(now);
        let deadline_ns = start
            + (now - start) / wait * wait
            + wait
            + limits::WRITE_QUEUE_GRACE.as_nanos() as Nanos;
        let seat = &mut self.seats[s as usize];
        if seat.served.is_none() {
            seat.polls.remove(&c);
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
        self.seats[s as usize].polls.remove(&c);
        self.metrics.incr("ctl_write_429");
        let d = self.backoff(c);
        self.engine.after(d, Ev::Poll(c, e));
    }

    /// A write on seat `s` finished: hand its slot to the oldest live
    /// queued poll. Expired queued polls learn their 429 here, at the next
    /// release, approximating their own timer.
    fn release_write(&mut self, s: u8, now: Nanos) {
        let mut expired = Vec::new();
        let (epoch, state) = (&self.epoch, &self.state);
        let next = self.seats[s as usize]
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
            self.seats[s as usize].polls.remove(&c);
            self.engine.at(now, Ev::Poll(c, e));
            return;
        };
        let after = Some(self.applied_seq[c as usize]).filter(|a| *a > 0);
        let image = self.image.as_ref().filter(|i| i.record.sequence == seq);
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
        let body = reply_body(base, after, self.held[c as usize]);
        if body == Body::Full && base.is_some_and(|(q, _)| Some(q) == after) {
            self.metrics.incr("ctl_base_hash_mismatches");
        }
        let hash = image.map_or_else(
            || self.hashes.get(&seq).copied().unwrap_or(0),
            |i| fingerprint(&i.record.content_hash),
        );
        let bytes = match body {
            Body::Delta => {
                self.metrics.incr("ctl_delta_replies");
                delta.map_or(0, |d| d.2.len() as u64)
            }
            Body::Full => {
                self.metrics.incr("ctl_full_replies");
                if let Some(a) = after {
                    self.metrics.incr("ctl_full_fallbacks");
                    // Skipped a version, holds the first (empty) publication,
                    // no delta from its version, or a hash mismatch.
                    self.metrics.incr(if seq > a + 1 {
                        "ctl_full_fallbacks_skipped"
                    } else if a == 1 && delta.is_none() {
                        "ctl_full_fallbacks_first"
                    } else if delta.is_none() {
                        "ctl_full_fallbacks_no_delta"
                    } else {
                        "ctl_full_fallbacks_other"
                    });
                }
                image.map_or(
                    self.committed.as_ref().map_or(0, |r| r.publication_bytes),
                    |i| i.encoded.len() as u64,
                )
            }
        };
        self.metrics.incr("ctl_client_replies");
        let write =
            self.settings.write_ns + bytes.saturating_mul(SECONDS) / self.settings.slot_bps.max(1);
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
        // First snapshot: the dataplane opens its peer listener, then
        // reports (`Session::mark_serving`).
        if old_seq == 0 && self.report_serving(c) {
            self.metrics.incr("ctl_reports");
        }
        // Lag from the commit, or from the client's start if later.
        if let Some(t) = self.commit_at.get(&seq) {
            let from = (*t).max(self.started_ns[c as usize]);
            self.metrics
                .observe("ctl_client_lag_ns", now.saturating_sub(from));
        }
        self.trace(now, 0xD2, u64::from(c), seq);
    }

    fn fault(&mut self, i: usize, now: Nanos) {
        self.trace(now, 0xF0, i as u64, 0);
        let primary = self.primary.holder.filter(|p| self.alive(*p));
        let choose = |sim: &mut Self, fraction: f64| -> Vec<usize> {
            let threshold = (fraction.clamp(0.0, 1.0) * (1u64 << 53) as f64) as u64;
            (0..sim.state.len())
                .filter(|_| sim.rng.next_u64() >> 11 < threshold)
                .collect()
        };
        match self.faults[i].clone() {
            CtlFault::CrashPrimary {
                restart_after_ns, ..
            } => {
                if let Some(p) = primary {
                    self.crash(p, restart_after_ns);
                }
            }
            CtlFault::CrashSeats {
                count,
                restart_after_ns,
                ..
            } => {
                let victims: Vec<u32> = (0..self.seats.len())
                    .filter_map(|i| self.seats[i].lease.holder)
                    .filter(|h| Some(*h) != self.primary.holder && self.alive(*h))
                    .take(count as usize)
                    .collect();
                for p in victims {
                    self.crash(p, restart_after_ns);
                }
            }
            CtlFault::Partition {
                until_ns, fraction, ..
            } => {
                for c in choose(self, fraction) {
                    self.partitioned[c] = true;
                }
                self.engine.at(until_ns, Ev::Heal(i as u32));
            }
            CtlFault::Slow {
                until_ns, fraction, ..
            } => {
                for c in choose(self, fraction) {
                    self.slow[c] = true;
                }
                self.engine.at(until_ns, Ev::Heal(i as u32));
            }
            CtlFault::Grow { count, .. } => {
                let n = count.min(self.settings.pods.saturating_sub(self.state.len() as u32));
                for _ in 0..n {
                    self.add_client();
                }
                self.metrics.add("ctl_pods_added", u64::from(n));
            }
            CtlFault::Shrink { count, .. } => {
                let n = self.state.len() as u64;
                let mut victims = Vec::new();
                let mut tries = 0;
                while victims.len() < count as usize && tries < 4 * count + 64 {
                    tries += 1;
                    let p = (self.rng.next_u64() % n) as u32;
                    if Some(p) != self.primary.holder
                        && self.state[p as usize] != State::Gone
                        && !victims.contains(&p)
                    {
                        victims.push(p);
                    }
                }
                for p in victims {
                    self.remove(p);
                }
            }
        }
    }

    /// Crash Pod `p` and its client process (fresh state on restart). Its
    /// Leases are left to expire; its Node and Pod objects stay.
    fn crash(&mut self, p: u32, restart_after_ns: Option<Nanos>) {
        self.pod_crash(p);
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

    /// Remove a Node and its Pod: the next primary pass drops the member.
    fn remove(&mut self, p: u32) {
        if self.alive(p) {
            self.pod_crash(p);
        }
        self.inventory.nodes.remove(&format!("n{p}"));
        self.inventory.pods.remove(&uuid(u64::from(p), 1));
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
            self.seats[self.seat[c] as usize].polls.remove(&p);
        }
        self.epoch[c] += 1;
        self.seat[c] = NO_SEAT;
        self.attempts[c] = 0;
        self.authorized[c] = 0;
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
        let bound = self.settings.converge_ns;
        if now < self.settled_ns.saturating_add(bound) {
            return;
        }
        let Some(required) = self
            .commits
            .iter()
            .rev()
            .find(|(t, ..)| *t + bound <= now)
            .map(|c| c.1)
        else {
            return;
        };
        let enrolling = self.bound();
        for c in 0..self.state.len() {
            if self.state[c] == State::Gone
                || !self.alive(c as u32)
                || self.started_ns[c] + enrolling > now
            {
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
    /// calls.
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
            .map(|s| s.waiters.capacity() * 8 + s.polls.len() * 16 + s.auth.len() * 200)
            .sum();
        let inventory = self.inventory.nodes.len() * 256 + self.inventory.pods.len() * 512;
        let image = self
            .image
            .as_ref()
            .map_or(0, |i| i.encoded.len() + i.publication.members.len() * 160);
        vec![
            (
                "ctl_model",
                self.pods.len() * (std::mem::size_of::<Pod>() + 64) + inventory + image,
            ),
            ("ctl_engine", self.engine.approximate_bytes()),
            ("ctl_clients", clients + seats),
            (
                "ctl_commits",
                self.commits.capacity() * 24 + self.hashes.capacity() * 24,
            ),
        ]
    }

    pub fn report(&mut self, scenario: &str, seed: u64) -> Value {
        let memory = self.memory();
        let total: usize = memory.iter().map(|(_, b)| b).sum();
        let pods = self.state.len().max(1);
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
                "enrolled": self.enrolled,
                "sequence": self.committed.as_ref().map(|r| r.sequence),
                "membership_version": self.committed_membership_version(),
                "primary": self.primary.holder,
                "ready_seats": self.ready_seats().count(),
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

    // Control plane: every Pod runs the real lottery, renew, contend and
    // follower steps; the primary runs `plan_pass` and `discovery` and
    // commits through the `Writer` fence. Reads and writes of one event are
    // atomic, so CAS writes on a version just read succeed.

    fn at(&self, ns: u64) -> Instant {
        self.base + Duration::from_nanos(ns)
    }

    fn wall_secs(&self) -> u64 {
        1_800_000_000 + self.engine.now() / SECONDS
    }

    fn wall_micro(&self) -> String {
        election::now_micro(
            SystemTime::UNIX_EPOCH
                + Duration::from_secs(1_800_000_000)
                + Duration::from_nanos(self.engine.now()),
        )
    }

    fn schedule(&mut self, at: u64, pod: u32, ev: PodEv) {
        let epoch = self.pods[pod as usize].epoch;
        self.engine.at(at, Ev::Pod(pod, epoch, ev));
    }

    fn after(&mut self, d: Duration, pod: u32, ev: PodEv) {
        self.engine.after(
            d.as_nanos() as u64,
            Ev::Pod(pod, self.pods[pod as usize].epoch, ev),
        );
    }

    fn alive(&self, pod: u32) -> bool {
        self.pods.get(pod as usize).is_some_and(|p| p.alive)
    }

    /// Sequence seat `index` last served, while its holder is alive and
    /// still holds the seat.
    fn seat_served(&self, index: usize) -> Option<u64> {
        let s = self.seats.get(index)?;
        let p = &self.pods[s.lease.holder? as usize];
        (p.alive && p.seat == Some(index as u8)).then_some(s.lease_served)?
    }

    /// The dataplane on `pod` reported (one API write; the annotation
    /// outlives restarts). A new annotation wakes a primary pass through its
    /// Pod watch. Returns whether it was new.
    fn report_serving(&mut self, pod: u32) -> bool {
        let Some(p) = self.inventory.pods.get_mut(&uuid(u64::from(pod), 1)) else {
            return false;
        };
        let new = !p.annotations.contains_key(annotations::REPORT);
        if new {
            p.annotations
                .insert(annotations::REPORT.to_owned(), REPORT.to_owned());
        }
        self.api_calls(1);
        if new
            && let Some(primary) = self.primary.holder
            && !self.wake_pending
        {
            self.wake_pending = true;
            self.after(DEBOUNCE, primary, PodEv::Wake);
        }
        new
    }

    fn pod_crash(&mut self, pod: u32) {
        let p = &mut self.pods[pod as usize];
        p.alive = false;
        p.epoch += 1;
        p.seat = None;
        p.renew = None;
        p.primary = None;
        if self.settings.node_monitor_grace_ns > 0 {
            let d = Duration::from_nanos(self.settings.node_monitor_grace_ns);
            self.after(d, pod, PodEv::NodeNotReady);
        }
    }

    /// Set Node `n{pod}`'s `Ready` condition, stamped now, if it changes.
    fn set_node_ready(&mut self, pod: u32, ready: bool) {
        let since = self.wall_secs();
        if let Some(n) = self.inventory.nodes.get_mut(&format!("n{pod}"))
            && n.ready.is_none_or(|r| r.ready != ready)
        {
            n.ready = Some(Readiness {
                ready,
                since_unix: Some(since),
            });
        }
    }

    #[cfg(test)]
    fn node_ready(&self, pod: u32) -> bool {
        self.inventory
            .nodes
            .get(&format!("n{pod}"))
            .is_some_and(|n| n.ready.is_none_or(|r| r.ready))
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

    fn pod_step(&mut self, pod: u32, epoch: u32, ev: PodEv) {
        if ev == PodEv::Wake {
            self.wake_pending = false;
        }
        let p = &self.pods[pod as usize];
        if p.epoch != epoch {
            return;
        }
        if ev == PodEv::NodeNotReady {
            // Fires for a dead Pod; a restart bumped the epoch.
            return self.set_node_ready(pod, false);
        }
        if !p.alive {
            return;
        }
        match ev {
            PodEv::Lottery => self.lottery(pod),
            PodEv::Attempt(i) => self.attempt(pod, i),
            PodEv::Renew => self.renew(pod),
            PodEv::Contend => self.contend(pod),
            PodEv::Follow => self.follow(pod),
            PodEv::FollowDone => self.follow_done(pod),
            PodEv::PrimaryRenew => self.primary_renew(pod),
            PodEv::Reconcile => self.reconcile(pod, true),
            PodEv::Wake => self.reconcile(pod, false),
            PodEv::NodeNotReady => {}
        }
        // At most one Pod holds a fresh primary term with a writable fence.
        let now = self.engine.now();
        let fresh = self
            .pods
            .iter()
            .filter(|p| {
                p.alive
                    && p.primary.as_ref().is_some_and(Writer::may_write)
                    && p.lease_fresh_until.is_some_and(|t| t > now)
            })
            .count();
        self.invariants
            .check("ctl_single_primary", fresh <= 1, now, Some(pod), || {
                format!("{fresh} fresh primaries")
            });
    }

    fn lottery(&mut self, pod: u32) {
        let now = self.at(self.engine.now());
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
            rules::Plan::Idle(d) | rules::Plan::NoVacancy(d) => self.after(d, pod, PodEv::Lottery),
            rules::Plan::Attempt { delay, index } => {
                self.after(delay, pod, PodEv::Attempt(index as u8))
            }
        }
    }

    fn attempt(&mut self, pod: u32, index: u8) {
        let ns = self.engine.now();
        self.schedule(ns, pod, PodEv::Lottery);
        if self.pods[pod as usize].seat.is_some() {
            return;
        }
        let now = self.at(ns);
        self.pods[pod as usize].lottery.begin();
        self.api_calls(1);
        let obs = self.observe(
            &self.seats[index as usize].lease,
            annotations::seat_lease(index as usize),
        );
        if !self.pods[pod as usize].lottery.observe(
            index as usize,
            &obs,
            now,
            &format!("n{pod}"),
            &identity(pod),
        ) {
            return;
        }
        self.api_calls(1);
        let spec = election::claim(&obs.spec, &format!("p{pod}"), &self.wall_micro());
        let seat = &mut self.seats[index as usize];
        seat.lease.rv += 1;
        seat.lease.spec = spec;
        seat.lease.holder = Some(pod);
        seat.lease.ready = None;
        seat.lease_served = None;
        let p = &mut self.pods[pod as usize];
        p.lottery.won();
        p.seat = Some(index);
        p.renew = Some(SeatRenew::new(now));
        p.lease_fresh_until = Some(ns + limits::LEASE_FRESHNESS.as_nanos() as u64);
        self.after(limits::LEASE_RENEW, pod, PodEv::Renew);
        self.follow_at(pod, ns);
        self.schedule(ns, pod, PodEv::Contend);
    }

    fn renew(&mut self, pod: u32) {
        let p = &self.pods[pod as usize];
        let (Some(index), Some(mut schedule)) = (p.seat, p.renew) else {
            return;
        };
        let ns = self.engine.now();
        let now = self.at(ns);
        let ready = p
            .follower
            .ready(now)
            .then(|| self.committed.as_ref().map(|r| r.sequence))
            .flatten();
        let inputs = rules::RenewInputs::from_view(
            &self.view,
            index as usize,
            &format!("n{pod}"),
            p.primary.is_some(),
            false,
            false,
            ready != self.seats[index as usize].lease.ready,
        );
        let lost = match schedule.decide(now, inputs) {
            rules::Action::Wait => false,
            rules::Action::Release => {
                self.api_calls(1);
                let lease = &mut self.seats[index as usize].lease;
                lease.rv += 1;
                lease.spec = election::release(&lease.spec);
                lease.holder = None;
                lease.ready = None;
                true
            }
            rules::Action::Renew => {
                self.api_calls(1);
                schedule.put_started(now);
                let wall = self.wall_micro();
                let lease = &mut self.seats[index as usize].lease;
                if lease.holder == Some(pod) {
                    lease.rv += 1;
                    lease.spec = election::renew(&lease.spec, &wall);
                    lease.ready = ready;
                    schedule.put_ok(now);
                    self.pods[pod as usize].lease_fresh_until =
                        Some(ns + limits::LEASE_FRESHNESS.as_nanos() as u64);
                    false
                } else {
                    // Taken over after expiry: the re-read shows another term.
                    self.api_calls(1);
                    let _ = schedule.conflict(Some(false), now);
                    true
                }
            }
        };
        let p = &mut self.pods[pod as usize];
        if lost {
            p.seat = None;
            p.renew = None;
            p.primary = None;
            return;
        }
        p.renew = Some(schedule);
        // Production also wakes on local changes; polling at the ready
        // interval stands in for that.
        let wait = schedule
            .next
            .saturating_duration_since(now)
            .clamp(Duration::from_millis(1), rules::READY_MIN_INTERVAL);
        self.after(wait, pod, PodEv::Renew);
    }

    fn contend(&mut self, pod: u32) {
        if self.pods[pod as usize].seat.is_none() {
            return;
        }
        let ns = self.engine.now();
        let wait = Contend::wait(&mut self.rng);
        self.after(wait, pod, PodEv::Contend);
        if self.pods[pod as usize].primary.is_some() {
            return;
        }
        self.api_calls(1);
        let obs = self.observe(&self.primary, annotations::PRIMARY_LEASE.into());
        let now = self.at(ns);
        if !self.pods[pod as usize].contend.observe(&obs, now, true) {
            return;
        }
        self.api_calls(1);
        self.primary.rv += 1;
        self.primary.spec = election::claim(&obs.spec, &format!("p{pod}"), &self.wall_micro());
        self.primary.holder = Some(pod);
        let transitions = u64::from(self.primary.spec.transitions);
        let last = self.last_terms;
        self.invariants.check(
            "ctl_terms_monotonic",
            transitions > last,
            ns,
            Some(pod),
            || format!("terms {last} -> {transitions}"),
        );
        self.last_terms = transitions;
        self.metrics.incr("ctl_primary_terms");
        self.trace(ns, 0xC1, transitions, 0);
        let term = Term {
            lease_uid: format!("lease-{}", self.primary.uid),
            transitions: self.primary.spec.transitions,
            holder: identity(pod),
        };
        let p = &mut self.pods[pod as usize];
        p.primary = Some(Writer::new(term));
        p.contend.term_ended();
        // In-memory, per term, like `primary.rs`.
        self.membership_changed = None;
        self.primary_changed();
        self.after(DEBOUNCE, pod, PodEv::Reconcile);
        self.after(limits::LEASE_RENEW, pod, PodEv::PrimaryRenew);
    }

    /// The primary Lease changed: every Pod watches it, and each seat
    /// follower wakes (`seat.rs` `follower` waits on `primary_lease`),
    /// reading at most once a second. A follower with a read in flight
    /// reads again once it completes.
    fn primary_changed(&mut self) {
        let ns = self.engine.now();
        self.view_changed_at = Some(self.at(ns));
        self.view.holder = self.primary.holder.map(identity);
        for i in 0..self.pods.len() {
            let p = &mut self.pods[i];
            if !p.alive || p.seat.is_none() {
                continue;
            }
            if p.reading.is_some() {
                p.woken_during_read = true;
                continue;
            }
            let at = (ns + WATCH_DELAY_NS).max(p.followed_at.map_or(0, |t| t + FOLLOW_FLOOR_NS));
            if at < p.follow_at {
                self.follow_at(i as u32, at);
            }
        }
    }

    fn primary_renew(&mut self, pod: u32) {
        if self.pods[pod as usize].primary.is_none() {
            return;
        }
        self.api_calls(1);
        if self.primary.holder != Some(pod) {
            self.pods[pod as usize].primary = None;
            return;
        }
        self.primary.rv += 1;
        self.primary.spec = election::renew(&self.primary.spec, &self.wall_micro());
        self.primary_changed();
        self.after(limits::LEASE_RENEW, pod, PodEv::PrimaryRenew);
    }

    /// One primary pass. A `periodic` pass schedules the next one.
    fn reconcile(&mut self, pod: u32, periodic: bool) {
        if self.pods[pod as usize].primary.is_none() {
            return;
        }
        // Seat reads, primary Lease and state reads.
        self.api_calls(annotations::MAX_SEATS as u64 + 2);
        let ns = self.engine.now();
        let current = self.committed.clone();
        let lease_uid = format!("lease-{}", self.primary.uid);
        let writer = self.pods[pod as usize].primary.as_mut().expect("primary");
        if writer.observe(current.as_ref(), Some(&lease_uid)).is_err() {
            self.pods[pod as usize].primary = None;
            return;
        }
        if periodic {
            self.after(limits::LEASE_RENEW, pod, PodEv::Reconcile);
        }
        let live: BTreeMap<usize, LeaseObservation> = self
            .seats
            .iter()
            .enumerate()
            .filter(|(_, s)| {
                s.lease.holder.is_some_and(|h| {
                    self.pods[h as usize].alive && self.pods[h as usize].seat.is_some()
                })
            })
            .map(|(i, s)| (i, self.observe(&s.lease, annotations::seat_lease(i))))
            .collect();
        let Ok(plan) = plan_pass(
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
                writer: self.pods[pod as usize].primary.as_ref().expect("primary"),
                wall_unix: self.wall_secs(),
                membership_changed_unix: self.membership_changed,
            },
            &mut self.rng,
        ) else {
            return;
        };
        let mut published = false;
        if plan.changed
            && current
                .as_ref()
                .is_none_or(|c| state::check_monotonic(c, &plan.next).is_ok())
        {
            self.api_calls(1);
            self.metrics.incr("ctl_commits");
            if current
                .as_ref()
                .is_none_or(|c| c.membership_version != plan.next.membership_version)
            {
                self.membership_changed = Some(self.wall_secs());
            }
            // A new sequence goes on the primary Lease
            // (`annotations::SEQUENCE`), which wakes the seat followers.
            published = current
                .as_ref()
                .is_none_or(|c| c.sequence != plan.next.sequence);
            self.committed = Some(plan.next);
        }
        if let Some(image) = plan.image
            && self
                .committed
                .as_ref()
                .is_some_and(|r| r.sequence == image.record.sequence)
        {
            self.image = Some(image);
        }
        if published {
            let seq = self.committed.as_ref().expect("committed").sequence;
            self.commit_at.insert(seq, ns);
            self.committed_at(ns);
        }
        let sequence = self.committed.as_ref().map(|r| r.sequence);
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
            self.api_calls(1);
            self.primary.rv += 1;
            self.primary_changed();
        }
        // The primary serves its own image.
        if let Some(r) = self.committed.clone() {
            let now = self.at(ns);
            let f = &mut self.pods[pod as usize].follower;
            let _ = f.install(r, now);
            if let Some(i) = &self.image {
                f.install_image(i.record.clone());
            }
        }
    }

    /// Schedule `pod`'s next follower read at `at`, superseding any other.
    fn follow_at(&mut self, pod: u32, at: u64) {
        self.pods[pod as usize].follow_at = at;
        self.schedule(at, pod, PodEv::Follow);
    }

    fn follow(&mut self, pod: u32) {
        let ns = self.engine.now();
        let p = &mut self.pods[pod as usize];
        if p.follow_at != ns {
            return;
        }
        p.follow_at = u64::MAX;
        if p.seat.is_none() {
            return;
        }
        if p.reading.is_some() {
            // One read at a time: that read's completion reads again.
            p.woken_during_read = true;
            return;
        }
        p.followed_at = Some(ns);
        p.reading = Some((self.committed.clone(), p.follower.installs));
        p.woken_during_read = false;
        self.api_calls(1);
        if self.settings.follow_read_ns == 0 {
            return self.follow_done(pod);
        }
        self.schedule(ns + self.settings.follow_read_ns, pod, PodEv::FollowDone);
    }

    /// The follower's read answered: install what it returned (confirmed as
    /// of the read's start, as `seat::follow_once` does), then wait for the
    /// next wake or `FOLLOW_INTERVAL`.
    fn follow_done(&mut self, pod: u32) {
        let ns = self.engine.now();
        let now = self.at(ns);
        let p = &mut self.pods[pod as usize];
        let Some((record, installs)) = p.reading.take() else {
            return;
        };
        let woken = std::mem::take(&mut p.woken_during_read);
        let Some(index) = p.seat else {
            return;
        };
        let is_primary = p.primary.is_some();
        let started = self.base + Duration::from_nanos(p.followed_at.unwrap_or(ns));
        let f = &mut p.follower;
        if let Some(r) =
            record.filter(|r| !rules::superseded(f.installs != installs, f.high_water.as_ref(), r))
        {
            if f.install(r.clone(), started).is_ok()
                && rules::need_image(&r, f.image.as_ref(), is_primary)
                && let Some(i) = &self.image
                && i.record.sequence == r.sequence
            {
                f.install_image(i.record.clone());
            }
            if f.ready(now) {
                self.seats[index as usize].lease_served = Some(r.sequence);
            }
        }
        let next = if woken {
            ns.max(p.followed_at.map_or(0, |t| t + FOLLOW_FLOOR_NS))
        } else {
            ns + limits::FOLLOW_INTERVAL.as_nanos() as u64
        };
        self.follow_at(pod, next);
    }
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
        assert!(checks(r, "ctl_single_primary") > 0);
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
        assert!(sim.committed.is_some());
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
    fn api_limit_refills_at_qps() {
        let mut a = ApiLimit {
            qps: 10.0,
            burst: 2.0,
            tokens: 2.0,
            at_ns: 0,
        };
        assert!(a.admit(0) && a.admit(0) && !a.admit(0));
        // 100 ms refills one token at 10 qps.
        assert!(a.admit(100 * MILLIS) && !a.admit(100 * MILLIS));
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

    const S: u64 = SECONDS;

    /// `pods` Pods that reported from the start, no faults, run `secs`.
    fn model(pods: u32, secs: u64, seed: u64, f: impl FnOnce(&mut Settings)) -> ControlSim {
        let mut s = Settings {
            pods,
            initial_pods: pods,
            report_at_start: true,
            ..Settings::default()
        };
        f(&mut s);
        let mut sim = ControlSim::new(s, seed, Vec::new());
        sim.advance_to(secs * S);
        sim
    }

    /// Restart crashed Pod `p` and its client now.
    fn restart(sim: &mut ControlSim, p: u32) {
        let (now, e) = (sim.now_ns(), sim.epoch[p as usize]);
        sim.engine.at(now, Ev::Restart(p));
        sim.engine.at(now, Ev::Enroll(p, e));
        sim.started_ns[p as usize] = now;
        sim.live_clients += 1;
        sim.advance_to(sim.now_ns());
    }

    fn members(sim: &ControlSim) -> usize {
        sim.image
            .as_ref()
            .map_or(0, |i| i.publication.members.len())
    }

    fn mv(sim: &ControlSim) -> u64 {
        sim.committed_membership_version().unwrap()
    }

    fn seq(sim: &ControlSim) -> Option<u64> {
        sim.committed.as_ref().map(|r| r.sequence)
    }

    fn holders(sim: &ControlSim) -> Vec<Option<u32>> {
        sim.seats.iter().map(|s| s.lease.holder).collect()
    }

    /// A live Pod that is neither the primary nor a seat holder.
    fn bystander(sim: &ControlSim) -> u32 {
        let h = holders(sim);
        (0..sim.pods.len() as u32)
            .find(|p| {
                Some(*p) != sim.primary.holder
                    && !h.contains(&Some(*p))
                    && sim.alive(*p)
                    && sim.node_ready(*p)
            })
            .unwrap()
    }

    /// Crash a bystander and step until the commit that drops it.
    fn crash_until_commit(sim: &mut ControlSim, limit: u64) -> u64 {
        let before = seq(sim);
        let victim = bystander(sim);
        sim.crash(victim, None);
        let mut t = sim.now_ns();
        while seq(sim) == before {
            t += S / 100;
            sim.advance_to(t);
            assert!(t < limit, "no commit");
        }
        sim.commit_at[&seq(sim).unwrap()]
    }

    #[test]
    fn cold_start_elects_seats_primary_and_publishes() {
        let sim = model(50, 240, 1, |_| {});
        let r = &sim.metrics;
        assert_eq!(r.get("ctl_primary_terms"), 1);
        assert!(sim.primary.holder.is_some());
        assert_eq!(
            holders(&sim).iter().flatten().count(),
            3,
            "{:?}",
            holders(&sim)
        );
        assert!(seq(&sim).is_some_and(|s| s >= 1));
        assert_eq!(sim.ready_seats().count(), 3);
        assert_eq!(sim.invariants.to_json()["hard_failures"], 0);
        assert!(r.get("ctl_client_replies") >= 50);
    }

    #[test]
    fn primary_failover_and_growth() {
        let mut sim = model(40, 240, 3, |_| {});
        let first = sim.primary.holder.unwrap();
        let before = seq(&sim).unwrap();
        sim.crash(first, None);
        sim.advance_to(sim.now_ns() + 180 * S);
        assert_ne!(sim.primary.holder, Some(first));
        assert!(sim.primary.holder.is_some());
        assert_eq!(sim.metrics.get("ctl_primary_terms"), 2);
        sim.add_client();
        sim.advance_to(sim.now_ns() + 60 * S);
        assert!(seq(&sim).unwrap() > before, "growth publishes");
        restart(&mut sim, first);
        sim.advance_to(sim.now_ns() + 60 * S);
        assert!(sim.alive(first));
        assert!(sim.memory()[0].1 > 0);
        assert_eq!(sim.invariants.to_json()["hard_failures"], 0);
    }

    #[test]
    fn crashed_node_leaves_membership_after_grace_and_returns() {
        let mut sim = model(30, 240, 5, |_| {});
        assert_eq!(members(&sim), 30);
        let v = mv(&sim);
        let victim = bystander(&sim);
        sim.crash(victim, None);
        let t = sim.now_ns();
        // Before the node monitor grace plus the readiness grace: kept.
        sim.advance_to(t + 39 * S);
        assert!(sim.node_ready(victim));
        sim.advance_to(t + 41 * S);
        assert!(!sim.node_ready(victim));
        sim.advance_to(t + 65 * S);
        assert_eq!(members(&sim), 30, "dropped inside the grace");
        assert_eq!(mv(&sim), v);
        // One pass period after both graces: gone, in one version.
        sim.advance_to(t + 85 * S);
        assert_eq!(members(&sim), 29);
        assert_eq!(mv(&sim), v + 1);
        restart(&mut sim, victim);
        assert!(sim.node_ready(victim));
        sim.advance_to(sim.now_ns() + 15 * S);
        assert_eq!(members(&sim), 30);
        assert_eq!(mv(&sim), v + 2);
    }

    #[test]
    fn quick_restart_keeps_membership() {
        let mut sim = model(30, 240, 5, |_| {});
        let v = mv(&sim);
        let victim = bystander(&sim);
        sim.crash(victim, None);
        let t = sim.now_ns();
        sim.advance_to(t + 30 * S);
        restart(&mut sim, victim);
        sim.advance_to(t + 200 * S);
        assert!(sim.node_ready(victim));
        assert_eq!(members(&sim), 30);
        assert_eq!(mv(&sim), v);
    }

    /// With reports only from serving dataplanes, a Pod joins after it
    /// reports, joins are batched by the hold, and seats are sized for every
    /// eligible Pod meanwhile. Clients are partitioned so only the test
    /// reports.
    #[test]
    fn pods_join_once_serving_in_batches() {
        let s = Settings {
            pods: 40,
            initial_pods: 40,
            ..Settings::default()
        };
        let cut = CtlFault::Partition {
            from_ns: 0,
            until_ns: 10_000 * S,
            fraction: 1.0,
        };
        let mut sim = ControlSim::new(s, 3, vec![cut]);
        sim.advance_to(120 * S);
        assert_eq!(members(&sim), 0, "nobody serves yet");
        assert!(seq(&sim).is_some(), "an empty membership is published");
        assert_eq!(sim.ready_seats().count(), 3);
        assert!(sim.report_serving(0));
        assert!(!sim.report_serving(0), "reported once");
        // Woken by the watch: the first member joins after the debounce.
        sim.advance_to(122 * S);
        assert_eq!(members(&sim), 1);
        let v = mv(&sim);
        for p in 1..40 {
            sim.report_serving(p);
            sim.advance_to(122 * S + u64::from(p) * S / 10);
        }
        // Inside the hold after the first join: nobody else yet.
        sim.advance_to(129 * S);
        assert_eq!(members(&sim), 1);
        sim.advance_to(160 * S);
        assert_eq!(members(&sim), 40);
        assert!(mv(&sim) <= v + 2, "joins are batched");
        // A crash and restart keeps the report: no new version.
        let v = mv(&sim);
        let victim = (0..40).find(|p| Some(*p) != sim.primary.holder).unwrap();
        sim.crash(victim, None);
        sim.advance_to(170 * S);
        restart(&mut sim, victim);
        sim.advance_to(240 * S);
        assert!(!sim.report_serving(victim), "report survives the restart");
        assert_eq!(mv(&sim), v);
    }

    /// The primary publishes each commit on its Lease, so every seat serves
    /// it within the follower's one-second read floor; with slow reads, a
    /// commit during a read is served by that read's completion plus at most
    /// one more read.
    #[test]
    fn seats_serve_a_commit_promptly() {
        for read in [20 * MILLIS, 2 * S] {
            let mut sim = model(30, 240, 5, |s| s.follow_read_ns = read);
            let mut overlapped = 0;
            for _ in 0..4 {
                let committed = crash_until_commit(&mut sim, 1_000 * S);
                overlapped += sim
                    .pods
                    .iter()
                    .filter(|p| p.alive && p.seat.is_some() && p.reading.is_some())
                    .count();
                sim.advance_to(committed + read.max(S) + read + S / 10);
                let served: Vec<usize> = (0..annotations::MAX_SEATS)
                    .filter(|i| sim.seats[*i].lease.holder.is_some())
                    .collect();
                assert!(!served.is_empty());
                for i in served {
                    assert_eq!(sim.seat_served(i), seq(&sim), "seat {i} read {read}");
                }
            }
            if read > S {
                assert!(overlapped > 0, "no commit overlapped a read");
            }
        }
    }

    #[test]
    fn accessors_and_removal() {
        let mut sim = model(30, 240, 5, |_| {});
        let s = seq(&sim).unwrap();
        assert_eq!(sim.image.as_ref().map(|i| i.record.sequence), Some(s));
        assert!(sim.commit_at.get(&s).is_some_and(|t| *t <= sim.now_ns()));
        let served = (0..annotations::MAX_SEATS)
            .filter_map(|i| sim.seat_served(i))
            .count();
        assert_eq!(served, sim.ready_seats().count());
        let v = mv(&sim);
        let victim = bystander(&sim);
        sim.remove(victim);
        assert!(!sim.alive(victim));
        sim.advance_to(sim.now_ns() + 60 * S);
        assert!(mv(&sim) > v);
        assert_eq!(sim.seat_served(99), None);
    }
}
