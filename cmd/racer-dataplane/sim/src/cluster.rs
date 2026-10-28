//! A compact, single-threaded model of the whole control plane, built from
//! the real steps, for the simulator.
//!
//! [`ControlPlane`] holds `n` dataplane Pods (one per Node). Each runs the
//! real [`Lottery`], [`SeatRenew`], [`Contend`], and [`Follower`] steps; the
//! primary runs the real [`plan_pass`] and [`discovery`] over typed
//! inventory, and commits through the real [`Writer`] fence. Leases and the
//! state record are typed values with resourceVersion counters rather than
//! JSON objects, and every API read or write is counted. Client long polls
//! are modeled per seat with the real write-slot limit and the
//! [`drain`] queue model, so the simulator sees 429s and delivery lag
//! at 50k Pods without one socket or thread per Pod.
//!
//! Time is `u64` nanoseconds from an `Instant` base chosen by the caller;
//! wall time is `wall_base + ns`. Randomness comes from one [`SplitMix64`]
//! stream, so runs replay exactly.

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
        primary::{Contend, Inventory, PassInputs, discovery, plan_pass},
        seat::{self as rules, Follower, Lottery, SeatRenew},
    },
};
use racer_core::time::SplitMix64;
use std::{
    cmp::Reverse,
    collections::{BTreeMap, BinaryHeap},
    time::{Duration, Instant, SystemTime},
};

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
    /// Model time of this Pod's next follower read; an `Ev::Follow` at any
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
enum Ev {
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
    queue: BinaryHeap<Reverse<(u64, u64, u32, Ev)>>,
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
    /// A [`Ev::Wake`] pass is queued.
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
            cp.schedule(0, i, Ev::Lottery(i));
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
            self.after(DEBOUNCE, primary, Ev::Wake(primary));
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

    fn schedule(&mut self, at: u64, pod: u32, ev: Ev) {
        self.seq += 1;
        let epoch = self.pods.get(pod as usize).map_or(0, |p| p.epoch);
        self.queue.push(Reverse((at, self.seq, epoch, ev)));
    }

    fn after(&mut self, d: Duration, pod: u32, ev: Ev) {
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
            self.after(grace, pod, Ev::NodeNotReady(pod));
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
        self.schedule(self.now, pod, Ev::Lottery(pod));
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
        self.schedule(self.now, i, Ev::Lottery(i));
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
                Ev::Lottery(p)
                | Ev::Attempt(p, _)
                | Ev::Renew(p)
                | Ev::Contend(p)
                | Ev::Follow(p)
                | Ev::FollowDone(p)
                | Ev::PrimaryRenew(p)
                | Ev::Reconcile(p)
                | Ev::NodeNotReady(p)
                | Ev::Wake(p) => p,
            };
            if let Ev::Wake(_) = ev {
                self.wake_pending = false;
            }
            let p = &self.pods[pod as usize];
            if p.epoch != epoch {
                continue;
            }
            if let Ev::NodeNotReady(p) = ev {
                // Fires for a dead Pod; a restart bumped the epoch.
                self.set_node_ready(p, false);
                continue;
            }
            if !p.alive {
                continue;
            }
            match ev {
                Ev::Lottery(p) => self.lottery(p),
                Ev::Attempt(p, i) => self.attempt(p, i),
                Ev::Renew(p) => self.renew(p),
                Ev::Contend(p) => self.contend(p),
                Ev::Follow(p) => self.follow(p),
                Ev::FollowDone(p) => self.follow_done(p),
                Ev::PrimaryRenew(p) => self.primary_renew(p),
                Ev::Reconcile(p) => self.reconcile(p, true),
                Ev::Wake(p) => self.reconcile(p, false),
                Ev::NodeNotReady(_) => {}
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
                self.after(d, pod, Ev::Lottery(pod))
            }
            rules::Plan::Attempt { delay, index } => {
                self.after(delay, pod, Ev::Attempt(pod, index as u8))
            }
        }
    }

    fn attempt(&mut self, pod: u32, index: u8) {
        self.schedule(self.now, pod, Ev::Lottery(pod));
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
        self.after(limits::LEASE_RENEW, pod, Ev::Renew(pod));
        self.follow_at(pod, self.now);
        self.schedule(self.now, pod, Ev::Contend(pod));
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
        self.after(wait, pod, Ev::Renew(pod));
    }

    fn contend(&mut self, pod: u32) {
        if self.pods[pod as usize].seat.is_none() {
            return;
        }
        let now = self.at(self.now);
        let wait = Contend::wait(&mut self.rng);
        if self.pods[pod as usize].primary.is_some() {
            self.after(wait, pod, Ev::Contend(pod));
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
            self.after(DEBOUNCE, pod, Ev::Reconcile(pod));
            self.after(limits::LEASE_RENEW, pod, Ev::PrimaryRenew(pod));
        }
        self.after(wait, pod, Ev::Contend(pod));
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
        self.after(limits::LEASE_RENEW, pod, Ev::PrimaryRenew(pod));
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
            self.after(limits::LEASE_RENEW, pod, Ev::Reconcile(pod));
        }
    }

    /// Schedule `pod`'s next follower read at `at`, superseding any other.
    fn follow_at(&mut self, pod: u32, at: u64) {
        self.pods[pod as usize].follow_at = at;
        self.schedule(at, pod, Ev::Follow(pod));
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
        self.schedule(at, pod, Ev::FollowDone(pod));
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

    fn run(pods: u32, secs: u64, seed: u64) -> ControlPlane {
        let mut cp = ControlPlane::new(pods, seed, true);
        cp.advance_to(secs * 1_000_000_000);
        cp
    }

    #[test]
    fn cold_start_elects_seats_primary_and_publishes() {
        let cp = run(50, 240, 1);
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
    fn replays_exactly() {
        assert_eq!(run(30, 120, 7).stats, run(30, 120, 7).stats);
    }

    #[test]
    fn primary_failover_and_growth() {
        let mut cp = run(40, 240, 3);
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
        let mut cp = run(30, 240, 5);
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
        let mut cp = run(30, 240, 5);
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
        let mut cp = run(30, 240, 5);
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
        let mut cp = run(30, 240, 5);
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
        let cp = run(50_000, 300, 1);
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
        let cp = run(600, 240, 3);
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
