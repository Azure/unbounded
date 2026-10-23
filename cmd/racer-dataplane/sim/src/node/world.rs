//! Event-driven read path over real node logic.
//!
//! One client page read at node `n`:
//!
//! 1. Dispatch to the worker `WorkerMap::select` picks; CPU `request_ns`.
//! 2. Memory index (`TickLru`), then disk index (`PhantomEntries`).
//! 3. Miss: join the worker's `PageFlights`. A waiter sleeps on a token; the
//!    leader runs a [`FillPlan`]: the placement ranking (phantom
//!    `Placement::rank_async`, polled to completion, one CPU ranking chunk per
//!    poll), then a [`CandidateWalk`]. Each walk `Probe` becomes a peer
//!    request sized by `plan_attempt`, routed hop by hop: every holder runs
//!    `decide_hop`, relays search their own route (`Paths<SharedRoutes>`) with
//!    the budget `search_budget`/`relay` allow, take a `Relay` quota, sign, and
//!    forward. Link failures feed the node's link circuits.
//! 4. The destination applies `serve_action`: copy-only probes check local
//!    tiers; acquire probes get `inherited_budget` and run the same flight and
//!    fill logic. Replies travel the reverse route; pages are cut-through
//!    transfers on the [`Fabric`].
//! 5. Only a walk that ends in `End::Origin` with validated authority reaches
//!    the origin. 412 continues with `RemainingWalk`; publication inserts the
//!    page into memory and applies the `persist` rule to write it to disk,
//!    reclaiming with the scored segment CLOCK.

use std::collections::HashMap;
use std::future::Future;
use std::pin::pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};
use std::time::{Duration, Instant};

use flow_control::coalesce::flight::state::Outcome as FlightOutcome;
use page_alloc::SegmentId;
use racer_core::cache::segment::{SCAN_BUDGET, page_value, unseen_value};
use racer_core::peer::Fault;
use racer_core::peer::hop::{
    Hop, HopRoute, RelayFailure, RelayPlan, Response, decide_hop, relay_failure,
};
use racer_core::read::Error;
use racer_core::read::budget::{AcquisitionBudget, inherited_budget};
use racer_core::read::candidates::{
    AuthorityFacts, OperationKind, ProbeOutcome, ReplicaSelection, ResponseKind, VisitOrder,
    classify, replica_score, validate_origin_authority, visit_order,
};
use racer_core::read::fill::{FillPlan, FillStep, Source, WalkResult};
use racer_core::read::flight::{Event as FlightEvent, Joined};
use racer_core::read::policy::{
    Persist, PersistFacts, peer_error, persist, recoverable, retention_eligible, retention_score,
};
use racer_core::read::range::{PageBudget, SlotKind, direct_fills, first_failure};
use racer_core::read::serve::{ServeAction, ServeFacts, serve_action, serve_deadline};
use racer_core::read::walk::{
    AttemptInputs, CandidateWalk, Continuation, End, HedgeFacts, Mode, Outcome, Probe,
    RemainingWalk, Step, WalkConfig, hedge_eligible, plan_attempt, refresh_allowed,
    transport_outcome,
};
use racer_core::route::MAX_LINKS;
use racer_core::time::SplitMix64;
use serde_json::{Value, json};
use topology::{PathQuery, Phantom, Placement, SharedRoutes};

use super::admission::{Admit, PAGE_BYTES, Rejections, ResourceClass, SimCharge, reserve};
use super::ids::Ids;
use super::membership::{Members, Versions, position};
use super::state::{Components, Disk, Node, Shared, record_bytes};
use crate::config::Config;
use crate::engine::{Engine, MILLIS, Nanos, Role, Streams, mix};
use crate::faults::{Fault as SimFault, Partitions, origin_errors_at};
use crate::report::{Invariants, Metrics, peak_rss_bytes};
use crate::resources::{Fabric, Origin, OriginOutcome};
use crate::scenario::{Fixed, MAX_ATTEMPTS};
use crate::workload::{Poisson, Reads, page_id};

/// Where a resolved page came from.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Got {
    Local,
    Peer,
    Origin,
    Joined,
}

/// One client page read.
#[derive(Clone, Copy, Debug)]
pub struct ClientRead {
    pub node: u32,
    pub incarnation: u32,
    pub page: u64,
    pub start: Nanos,
    pub attempt: u8,
    /// Range read id, 0 for a single page.
    pub range: u64,
    pub slot: u16,
}

/// Who wants a page at a node.
#[derive(Clone, Copy, Debug)]
pub enum Want {
    Client(ClientRead),
    /// A peer acquire request waiting at its destination.
    Peer {
        msg: u64,
    },
}

/// A reply as the requester sees it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Reply {
    /// The exchange failed without a signed response.
    Transport,
    Response(ResponseKind),
}

#[derive(Clone, Copy, Debug)]
pub enum Event {
    Arrive {
        node: u32,
    },
    Read(ClientRead),
    Hop {
        msg: u64,
    },
    LinkDown {
        msg: u64,
        holder: u32,
    },
    Reply {
        probe: u64,
        reply: Reply,
    },
    ProbeTimeout {
        probe: u64,
    },
    OriginDone {
        fill: u64,
        outcome: OriginOutcome,
    },
    Resolve {
        want: Want,
        result: Result<Got, Error>,
    },
    Written {
        write: u64,
    },
    Publish,
    Membership {
        node: u32,
        live: bool,
    },
    Crash {
        node: u32,
    },
    Restart {
        node: u32,
    },
    Slow {
        node: u32,
        factor: u8,
    },
}

impl Event {
    fn tag(&self) -> (u64, u64, u64) {
        match *self {
            Self::Arrive { node } => (1, node.into(), 0),
            Self::Read(r) => (2, r.node.into(), r.page),
            Self::Hop { msg } => (3, msg, 0),
            Self::LinkDown { msg, holder } => (4, msg, holder.into()),
            Self::Reply { probe, reply } => (
                5,
                probe,
                match reply {
                    Reply::Transport => 0,
                    Reply::Response(kind) => 1 + kind as u64,
                },
            ),
            Self::ProbeTimeout { probe } => (6, probe, 0),
            Self::OriginDone { fill, outcome } => (7, fill, outcome as u64),
            Self::Resolve { want, result } => (
                8,
                match want {
                    Want::Client(r) => r.page,
                    Want::Peer { msg } => msg,
                },
                result.is_ok().into(),
            ),
            Self::Written { write } => (9, write, 0),
            Self::Publish => (10, 0, 0),
            Self::Membership { node, live } => (11, node.into(), live.into()),
            Self::Crash { node } => (12, node.into(), 0),
            Self::Restart { node } => (13, node.into(), 0),
            Self::Slow { node, factor } => (14, node.into(), factor.into()),
        }
    }
}

enum Walking {
    None,
    Candidates(CandidateWalk),
    Remaining(RemainingWalk),
}

struct Fill {
    node: u32,
    incarnation: u32,
    worker: u8,
    page: u64,
    leader: u64,
    token: u64,
    plan: FillPlan,
    budget: AcquisitionBudget,
    scope: Instant,
    walk: Walking,
    cands: Vec<u32>,
    version: u64,
    rank: Option<usize>,
    retried: bool,
    probe: Option<u64>,
    _reservation: Vec<SimCharge>,
}

struct Waiting {
    node: u32,
    incarnation: u32,
    worker: u8,
    page: u64,
    id: u64,
    want: Want,
}

struct Msg {
    probe: u64,
    requester: u32,
    /// Current holder (the node the message is at or travelling to).
    at: u32,
    dest: u32,
    version: u64,
    page: u64,
    acquire: bool,
    /// Wire form: the requester and every relay so far.
    path: Vec<u32>,
    links: u8,
    attempts: u32,
    deadline: Nanos,
    relays: Vec<SimCharge>,
}

struct Probing {
    fill: u64,
    acquire: bool,
}

struct RangeRead {
    node: u32,
    first: u64,
    start: Nanos,
    offset: usize,
    slots: Vec<SlotKind>,
    incarnation: u32,
}

fn err_name(e: Error) -> &'static str {
    match e {
        Error::InvalidConfiguration => "error_invalid_configuration",
        Error::InvalidRequest => "error_invalid_request",
        Error::HeaderTooLarge => "error_header_too_large",
        Error::NotFound => "error_not_found",
        Error::Internal => "error_internal",
        Error::VersionUnavailable => "error_version_unavailable",
        Error::Unavailable => "error_unavailable",
        Error::Overloaded => "error_overloaded",
        Error::DeadlineExceeded => "error_deadline_exceeded",
        Error::Cancelled => "error_cancelled",
        Error::Unauthorized => "error_unauthorized",
        Error::OriginRejected => "error_origin_rejected",
        Error::OriginForbidden => "error_origin_forbidden",
        Error::IncompatibleMembership => "error_incompatible_membership",
        Error::HopBudgetExhausted => "error_hop_budget_exhausted",
        Error::CorruptRecord => "error_corrupt_record",
        Error::MissingKey => "error_missing_key",
        Error::Io => "error_io",
        Error::Os(_) => "error_os",
    }
}

fn response_kind(r: Response) -> ResponseKind {
    match r {
        Response::Overloaded => ResponseKind::Overloaded,
        Response::Unavailable => ResponseKind::Unavailable,
        Response::NotFound => ResponseKind::NotFound,
        Response::VersionUnavailable => ResponseKind::VersionUnavailable,
        Response::OriginRejected => ResponseKind::OriginRejected,
        Response::OriginForbidden => ResponseKind::OriginForbidden,
        Response::StaleMembership => ResponseKind::StaleMembership,
    }
}

/// Poll a future that never waits on I/O to completion with a no-op waker,
/// returning its output and the number of polls (work quanta).
fn drive<F: Future>(future: F) -> (F::Output, u64) {
    let mut future = pin!(future);
    let mut cx = Context::from_waker(Waker::noop());
    let mut polls = 0;
    loop {
        polls += 1;
        if let Poll::Ready(out) = future.as_mut().poll(&mut cx) {
            return (out, polls);
        }
    }
}

/// Link retry backoff, shaped like production `backoff` (`src/topology.rs`):
/// 100 ms doubling per failure up to 20 s, plus up to a quarter of jitter. The
/// jitter comes from a fixed mix instead of SHA-256 of the node name.
fn link_backoff(node: &u32, failures: u32) -> Duration {
    let base = (100u64 << failures.saturating_sub(1).min(8)).min(20_000);
    let jitter = mix((u64::from(*node) << 32) | u64::from(failures)) % (base / 4 + 1);
    Duration::from_millis(base + jitter)
}

pub struct NodeWorld {
    pub config: Config,
    pub engine: Engine<Event>,
    pub fabric: Fabric,
    pub origin: Origin,
    pub metrics: Metrics,
    pub invariants: Invariants,
    pub versions: Versions,
    partitions: Partitions,
    ids: Ids,
    shared: Shared,
    nodes: Vec<Node>,
    node_rng: Vec<SplitMix64>,
    reads: Reads,
    arrivals: Poisson,
    base: Instant,
    next_id: u64,
    fills: HashMap<u64, Fill, Fixed>,
    tokens: HashMap<u64, Waiting, Fixed>,
    msgs: HashMap<u64, Msg, Fixed>,
    probes: HashMap<u64, Probing, Fixed>,
    ranges: HashMap<u64, RangeRead, Fixed>,
    writes: HashMap<u64, SimCharge, Fixed>,
    origin_per_page: HashMap<u64, u32, Fixed>,
    publish_scheduled: bool,
}

impl NodeWorld {
    pub fn new(config: Config, seed: u64) -> Self {
        let streams = Streams::new(seed);
        let fabric = Fabric::new(&config, &mut streams.stream(Role::Topology, 0));
        let origin = Origin::new(&config, streams.stream(Role::Origin, 0));
        let n = config.nodes;
        // Instants are only used as offsets from this base, so runs replay.
        #[allow(clippy::disallowed_methods)]
        let base = Instant::now();
        let shared = Shared {
            phantom: Phantom::new(),
            routes: SharedRoutes::new(),
            rejections: Rejections::default(),
        };
        let node_config = config.node;
        let nodes = (0..n)
            .map(|_| {
                Node::new(
                    &node_config,
                    config.workers,
                    Disk::new(
                        config.disk_pages,
                        node_config.pages_per_segment,
                        config.page_bytes,
                    ),
                    &shared,
                    config.disk_pages,
                    base,
                )
            })
            .collect();
        let mut world = Self {
            engine: Engine::new(),
            fabric,
            origin,
            metrics: Metrics::default(),
            invariants: Invariants::default(),
            versions: Versions::new(
                n,
                node_config.membership_lag_ns,
                node_config.retained_versions,
            ),
            partitions: Partitions::from_faults(&config.faults),
            ids: Ids::new(n, config.workers.max(1), config.pages_per_object),
            shared,
            nodes,
            node_rng: (0..n)
                .map(|i| streams.stream(Role::Node, i.into()))
                .collect(),
            reads: Reads::new(
                config.objects,
                config.zipf_s,
                config.pages_per_object,
                config.range_fraction,
                config.range_pages,
            ),
            arrivals: Poisson::new(config.request_rate),
            base,
            next_id: 0,
            fills: HashMap::default(),
            tokens: HashMap::default(),
            msgs: HashMap::default(),
            probes: HashMap::default(),
            ranges: HashMap::default(),
            writes: HashMap::default(),
            origin_per_page: HashMap::default(),
            publish_scheduled: false,
            config,
        };
        world.schedule_initial(&streams);
        world
    }

    fn schedule_initial(&mut self, streams: &Streams) {
        for node in 0..self.config.nodes {
            if let Some(gap) = self.arrivals.gap(&mut self.node_rng[node as usize]) {
                self.engine.at(gap, Event::Arrive { node });
            }
        }
        let mut storm_rng = streams.stream(Role::Workload, 1);
        for storm in self.config.storms.clone() {
            for (at, node) in storm.expand(self.config.nodes, &mut storm_rng) {
                for p in 0..self.config.pages_per_object {
                    let page = page_id(storm.object, p, self.config.pages_per_object);
                    self.engine.at(
                        at,
                        Event::Read(ClientRead {
                            node,
                            incarnation: 0,
                            page,
                            start: at,
                            attempt: 0,
                            range: 0,
                            slot: 0,
                        }),
                    );
                }
            }
        }
        for c in self.config.churn.clone() {
            self.engine.at(
                c.at_ns,
                Event::Membership {
                    node: c.node,
                    live: c.join,
                },
            );
        }
        for f in self.config.faults.clone() {
            match f {
                SimFault::Crash {
                    node,
                    at_ns,
                    restart_ns,
                } => {
                    self.engine.at(at_ns, Event::Crash { node });
                    self.engine
                        .at(restart_ns.max(at_ns), Event::Restart { node });
                }
                SimFault::Slow {
                    node,
                    factor,
                    from_ns,
                    until_ns,
                } => {
                    self.engine.at(from_ns, Event::Slow { node, factor });
                    self.engine
                        .at(until_ns.max(from_ns), Event::Slow { node, factor: 1 });
                }
                SimFault::Partition { .. } | SimFault::OriginErrors { .. } => {}
            }
        }
    }

    pub fn run(&mut self, end: Nanos) {
        while let Some(event) = self.engine.pop_until(end) {
            let now = self.engine.now();
            let (tag, a, b) = event.tag();
            self.engine.trace.record(now, tag, a, b);
            self.handle(event);
        }
        self.engine.finish(end);
    }

    fn now(&self) -> Nanos {
        self.engine.now()
    }

    fn instant(&self, t: Nanos) -> Instant {
        self.base + Duration::from_nanos(t)
    }

    fn nanos(&self, i: Instant) -> Nanos {
        i.saturating_duration_since(self.base).as_nanos() as Nanos
    }

    fn fresh(&mut self) -> u64 {
        self.next_id += 1;
        self.next_id
    }

    fn valid(&self, node: u32) -> bool {
        node < self.config.nodes
    }

    fn handle(&mut self, event: Event) {
        match event {
            Event::Arrive { node } => self.arrive(node),
            Event::Read(r) => self.read(r),
            Event::Hop { msg } => self.hop(msg),
            Event::LinkDown { msg, holder } => self.fail_hop(msg, holder, Fault::Unavailable),
            Event::Reply { probe, reply } => self.reply(probe, reply),
            Event::ProbeTimeout { probe } => {
                if let Some(p) = self.probes.remove(&probe) {
                    self.metrics.incr("probe_timeouts");
                    self.complete_probe(p.fill, Ok(Outcome::Transport(ProbeOutcome::Unreachable)));
                }
            }
            Event::OriginDone { fill, outcome } => self.origin_done(fill, outcome),
            Event::Resolve { want, result } => self.resolve(want, result),
            Event::Written { write } => {
                self.writes.remove(&write);
            }
            Event::Publish => self.publish(),
            Event::Membership { node, live } => {
                if self.versions.set(node, live) {
                    self.metrics.incr("membership_changes");
                    if !self.publish_scheduled {
                        self.publish_scheduled = true;
                        let batch = self.config.node.membership_batch_ns;
                        self.engine
                            .at((self.now() / batch + 1) * batch, Event::Publish);
                    }
                }
            }
            Event::Crash { node } => self.crash(node),
            Event::Restart { node } => {
                if self.valid(node) && self.nodes[node as usize].down {
                    self.nodes[node as usize].down = false;
                    self.fabric.reset_node(node, self.now());
                    self.metrics.incr("restarts");
                }
            }
            Event::Slow { node, factor } => {
                if self.valid(node) {
                    self.fabric.slow[node as usize] = factor.max(1);
                }
            }
        }
    }

    fn publish(&mut self) {
        self.publish_scheduled = false;
        let now = self.now();
        let dropped = self.versions.publish(now);
        self.metrics.incr("membership_publications");
        if dropped {
            self.shared.phantom.forget(&self.versions.identities());
            self.shared
                .routes
                .forget(&self.versions.topology_identities());
        }
    }

    fn crash(&mut self, node: u32) {
        if !self.valid(node) || self.nodes[node as usize].down {
            return;
        }
        self.metrics.incr("crashes");
        let mut dead: Vec<u64> = self
            .fills
            .iter()
            .filter(|(_, f)| f.node == node)
            .map(|(id, _)| *id)
            .collect();
        dead.sort_unstable();
        for id in dead {
            if let Some(fill) = self.fills.remove(&id)
                && let Some(probe) = fill.probe
            {
                self.probes.remove(&probe);
            }
        }
        self.tokens.retain(|_, w| w.node != node);
        self.ranges.retain(|_, r| r.node != node);
        let now = self.instant(self.now());
        let node_config = self.config.node;
        let n = &mut self.nodes[node as usize];
        n.restart(&node_config, &self.shared, self.config.disk_pages, now);
        n.down = true;
    }

    /// Adopt the newest membership version visible to `node` now, running
    /// placement maintenance on every worker when it changes.
    fn adopt(&mut self, node: u32) -> u64 {
        let now = self.now();
        let version = self.versions.adopted(node, now);
        let (number, members) = (version.number, version.members.clone());
        let old = self.nodes[node as usize].version;
        if number == old {
            return number;
        }
        self.invariants.check(
            "membership_version_monotonic",
            number > old,
            now,
            Some(node),
            || format!("adopted version {number} after {old}"),
        );
        self.metrics.incr("membership_adoptions");
        let costs = self.fabric.costs;
        let n = &mut self.nodes[node as usize];
        n.version = number.max(old);
        for (w, worker) in n.workers.iter().enumerate() {
            let mut steps = 0u64;
            while steps < 4096 {
                match worker.placement.maintain(&*members) {
                    topology::Maintenance::Progress => steps += 1,
                    topology::Maintenance::Idle | topology::Maintenance::Blocked => break,
                }
            }
            if steps > 0 {
                self.metrics.add("placement_maintenance_steps", steps);
                self.fabric
                    .cpu(now, node, w as u8, steps * costs.ranking_chunk_ns);
            }
        }
        number
    }

    // ---- client reads ----

    fn arrive(&mut self, node: u32) {
        if !self.valid(node) {
            return;
        }
        let now = self.now();
        let rng = &mut self.node_rng[node as usize];
        if let Some(gap) = self.arrivals.gap(rng) {
            self.engine.after(gap, Event::Arrive { node });
        }
        if self.nodes[node as usize].down {
            return;
        }
        if !self.versions.is_live(node) {
            self.metrics.incr("reads_skipped_not_member");
            return;
        }
        let read = self.reads.draw(rng);
        let first = page_id(read.object, read.first, self.config.pages_per_object);
        let incarnation = self.nodes[node as usize].incarnation;
        if read.pages <= 1 {
            self.read(ClientRead {
                node,
                incarnation,
                page: first,
                start: now,
                attempt: 0,
                range: 0,
                slot: 0,
            });
            return;
        }
        self.metrics.incr("range_reads");
        let id = self.fresh();
        self.ranges.insert(
            id,
            RangeRead {
                node,
                first,
                start: now,
                offset: 0,
                slots: vec![SlotKind::Missing { select: false }; read.pages as usize],
                incarnation,
            },
        );
        self.schedule_range(id);
    }

    /// Start the slots `direct_fills` plans for a range read.
    fn schedule_range(&mut self, id: u64) {
        let now = self.now();
        let Some(range) = self.ranges.get_mut(&id) else {
            return;
        };
        while range.offset < range.slots.len() && range.slots[range.offset] == SlotKind::Ready {
            range.offset += 1;
        }
        let window = &range.slots[range.offset..];
        if window.is_empty() || first_failure(window) == Some(0) {
            let ok = window.is_empty();
            let range = self.ranges.remove(&id).expect("present");
            if ok {
                self.metrics.incr("range_reads_completed");
                self.metrics.observe("range_read_ns", now - range.start);
            } else {
                self.metrics.incr("range_reads_failed");
            }
            return;
        }
        let plan = direct_fills(window, |_| false);
        let offset = range.offset;
        let mut start = Vec::with_capacity(plan.len());
        for i in plan {
            let slot = offset + i;
            if matches!(range.slots[slot], SlotKind::Missing { .. }) {
                range.slots[slot] = SlotKind::Fetching;
                start.push(ClientRead {
                    node: range.node,
                    incarnation: range.incarnation,
                    page: range.first + slot as u64,
                    start: now,
                    attempt: 0,
                    range: id,
                    slot: slot as u16,
                });
            }
        }
        for r in start {
            self.read(r);
        }
    }

    fn range_slot_done(&mut self, r: ClientRead, ok: bool) {
        if r.range == 0 {
            return;
        }
        if let Some(range) = self.ranges.get_mut(&r.range) {
            range.slots[r.slot as usize] = if ok {
                SlotKind::Ready
            } else {
                SlotKind::Failed
            };
            self.schedule_range(r.range);
        }
    }

    fn read(&mut self, r: ClientRead) {
        let node = r.node;
        if !self.valid(node) {
            return;
        }
        let n = &self.nodes[node as usize];
        if n.down || n.incarnation != r.incarnation {
            self.metrics.incr("reads_lost_to_crash");
            return;
        }
        self.adopt(node);
        let now = self.now();
        let deadline = self.instant(r.start + self.config.node.read_timeout_ns);
        match (PageBudget::Client { deadline }).next_page(self.instant(now)) {
            Ok(budget) => self.want(node, r.page, Want::Client(r), budget, true),
            Err(e) => self.resolve(Want::Client(r), Err(e)),
        }
    }

    // ---- local tiers and flights ----

    fn want(
        &mut self,
        node: u32,
        page: u64,
        want: Want,
        budget: AcquisitionBudget,
        plaintext: bool,
    ) {
        let now = self.now();
        let w = self.ids.worker(page);
        let costs = self.fabric.costs;
        let t = self.fabric.cpu(now, node, w, costs.request_ns);
        let peer = matches!(want, Want::Peer { .. });
        let n = &mut self.nodes[node as usize];
        let worker = &mut n.workers[w as usize];
        if worker.memory.pages.contains_key(&page) {
            let _ = worker.memory.touch(&page);
            self.metrics.incr(if peer {
                "peer_memory_hits"
            } else {
                "memory_hits"
            });
            self.resolve_at(want, Ok(Got::Local), t);
            return;
        }
        if n.disk.entries.get(&page).is_some() {
            n.retention
                .heat
                .touch(&page, self.base + Duration::from_nanos(now));
            self.metrics
                .incr(if peer { "peer_disk_hits" } else { "disk_hits" });
            let read = self
                .fabric
                .disk_read(t, node, record_bytes(self.config.page_bytes));
            self.memory_insert(node, w, page);
            self.resolve_at(want, Ok(Got::Local), read);
            return;
        }
        self.metrics.incr(if peer {
            "peer_local_misses"
        } else {
            "local_misses"
        });
        let page_id = self.ids.page(page);
        let instant = self.instant(now);
        let mut wakes = Vec::new();
        let n = &mut self.nodes[node as usize];
        let incarnation = n.incarnation;
        let worker = &mut n.workers[w as usize];
        let mut admit = Admit {
            quotas: &worker.quotas,
            charges: &mut worker.charges,
            ids: &self.ids,
        };
        let joined = worker.flights.join(
            &page_id,
            instant,
            budget.deadline(),
            budget.remaining_attempts(),
            plaintext,
            &mut admit,
            &mut wakes,
        );
        match joined {
            Err(e) => {
                self.metrics.incr("flight_join_rejected");
                self.resolve_at(want, Err(e), t);
            }
            Ok(Joined::Complete(_) | Joined::Partial(_)) => {
                self.metrics.incr("flight_completed_joins");
                self.resolve_at(want, Ok(Got::Joined), t);
            }
            Ok(Joined::Waiter(id)) => {
                self.next_id += 1;
                let token = self.next_id;
                let event = worker
                    .flights
                    .poll(&page_id, id, instant, token, &mut wakes);
                self.tokens.insert(
                    token,
                    Waiting {
                        node,
                        incarnation,
                        worker: w,
                        page,
                        id,
                        want,
                    },
                );
                match event {
                    Ok(FlightEvent::Lead) => {
                        self.start_fill(node, w, page, id, token, budget, plaintext);
                    }
                    Ok(FlightEvent::Pending) => self.metrics.incr("flight_joins"),
                    Ok(FlightEvent::Complete(_) | FlightEvent::Failed(_)) => wakes.push(token),
                    Err(e) => {
                        self.tokens.remove(&token);
                        self.detach(node, w, page, id);
                        self.resolve_at(want, Err(e), t);
                    }
                }
            }
        }
        for token in wakes {
            self.wake(token);
        }
    }

    fn detach(&mut self, node: u32, w: u8, page: u64, id: u64) {
        let page_id = self.ids.page(page);
        let mut wakes = Vec::new();
        let worker = &mut self.nodes[node as usize].workers[w as usize];
        let mut admit = Admit {
            quotas: &worker.quotas,
            charges: &mut worker.charges,
            ids: &self.ids,
        };
        worker.flights.detach(&page_id, id, &mut admit, &mut wakes);
        for token in wakes {
            self.wake(token);
        }
    }

    fn wake(&mut self, token: u64) {
        let Some(waiting) = self.tokens.remove(&token) else {
            return;
        };
        let n = &self.nodes[waiting.node as usize];
        if n.incarnation != waiting.incarnation {
            return;
        }
        let page_id = self.ids.page(waiting.page);
        let now = self.instant(self.now());
        let mut wakes = Vec::new();
        let worker = &mut self.nodes[waiting.node as usize].workers[waiting.worker as usize];
        let event = worker
            .flights
            .poll(&page_id, waiting.id, now, token, &mut wakes);
        let result = match event {
            Ok(FlightEvent::Pending) => {
                self.tokens.insert(token, waiting);
                return;
            }
            Ok(FlightEvent::Complete(_)) => Ok(Got::Joined),
            Ok(FlightEvent::Failed(e)) | Err(e) => Err(e),
            Ok(FlightEvent::Lead) => {
                // Only `Outcome::Retry` re-elects, and the simulator never
                // retries a flight, so this is unexpected.
                self.metrics.incr("flight_unexpected_lead");
                worker.flights.cancel(&page_id, waiting.id, &mut wakes);
                Err(Error::Unavailable)
            }
        };
        self.detach(waiting.node, waiting.worker, waiting.page, waiting.id);
        self.resolve(waiting.want, result);
        for token in wakes {
            self.wake(token);
        }
    }

    fn resolve_at(&mut self, want: Want, result: Result<Got, Error>, at: Nanos) {
        if at <= self.now() {
            self.resolve(want, result);
        } else {
            self.engine.at(at, Event::Resolve { want, result });
        }
    }

    fn resolve(&mut self, want: Want, result: Result<Got, Error>) {
        let now = self.now();
        match want {
            Want::Client(mut r) => {
                let n = &self.nodes[r.node as usize];
                if n.down || n.incarnation != r.incarnation {
                    self.metrics.incr("reads_lost_to_crash");
                    return;
                }
                match result {
                    Ok(got) => {
                        let open = self.fabric.crypto(
                            now,
                            r.node,
                            self.fabric.costs.aead(self.config.page_bytes),
                        );
                        let latency = open - r.start;
                        self.metrics.incr("reads_completed");
                        self.metrics.observe("read_ns", latency);
                        self.metrics.observe(
                            match got {
                                Got::Local => "read_local_ns",
                                Got::Peer => "read_peer_ns",
                                Got::Origin => "read_origin_ns",
                                Got::Joined => "read_joined_ns",
                            },
                            latency,
                        );
                        self.range_slot_done(r, true);
                    }
                    Err(e) => {
                        if recoverable(e) && r.attempt + 1 < MAX_ATTEMPTS {
                            self.metrics.incr("read_retries");
                            r.attempt += 1;
                            let backoff =
                                2 * self.fabric.rtt.rtt(0, 0).max(MILLIS) * u64::from(r.attempt);
                            self.engine.after(backoff, Event::Read(r));
                        } else {
                            self.metrics.incr("reads_failed");
                            self.metrics.incr(err_name(e));
                            self.range_slot_done(r, false);
                        }
                    }
                }
            }
            Want::Peer { msg } => {
                let Some(m) = self.msgs.get(&msg) else {
                    return;
                };
                let dest = m.dest;
                match result {
                    Ok(_) => self.send_reply(msg, dest, Reply::Response(ResponseKind::Page), now),
                    Err(e) => {
                        let reply = peer_error(e).map_or(Reply::Transport, Reply::Response);
                        self.send_reply(msg, dest, reply, now);
                    }
                }
            }
        }
    }

    fn memory_insert(&mut self, node: u32, w: u8, page: u64) {
        let cap = (self.config.memory_pages / u32::from(self.fabric.workers)).max(1) as usize;
        let memory = &mut self.nodes[node as usize].workers[w as usize].memory;
        if memory.pages.contains_key(&page) {
            let _ = memory.touch(&page);
            return;
        }
        while memory.len() >= cap {
            let Some(victim) = memory.victim(|_| true) else {
                break;
            };
            memory.remove(&victim);
            self.metrics.incr("memory_evictions");
        }
        if memory.insert(page, self.config.page_bytes as u32).is_err() {
            self.metrics.incr("memory_ticks_exhausted");
        }
    }

    // ---- fill ----

    #[allow(clippy::too_many_arguments)]
    fn start_fill(
        &mut self,
        node: u32,
        w: u8,
        page: u64,
        leader: u64,
        token: u64,
        budget: AcquisitionBudget,
        plaintext: bool,
    ) {
        let id = self.fresh();
        let quotas = &self.nodes[node as usize].workers[w as usize].quotas;
        let mut reservation = Vec::with_capacity(2);
        let mut failed = None;
        let classes: &[(ResourceClass, usize)] = if plaintext {
            &[
                (ResourceClass::Plaintext, PAGE_BYTES),
                (ResourceClass::Ciphertext, PAGE_BYTES + 16),
            ]
        } else {
            &[(ResourceClass::Ciphertext, PAGE_BYTES + 16)]
        };
        for (class, amount) in classes {
            match reserve(quotas, *class, *amount) {
                Ok(charge) => reservation.push(charge),
                Err(e) => {
                    failed = Some(e);
                    break;
                }
            }
        }
        let scope = budget.deadline();
        let mut plan = FillPlan::new(plaintext);
        plan.local(false);
        self.fills.insert(
            id,
            Fill {
                node,
                incarnation: self.nodes[node as usize].incarnation,
                worker: w,
                page,
                leader,
                token,
                plan,
                budget,
                scope,
                walk: Walking::None,
                cands: Vec::new(),
                version: 0,
                rank: None,
                retried: false,
                probe: None,
                _reservation: reservation,
            },
        );
        self.metrics.incr("fills");
        match failed {
            Some(e) => self.finish_fill(id, Err(e)),
            None => self.begin_walk(id),
        }
    }

    /// Rank `key` with the worker's phantom placement, charging one ranking
    /// chunk of CPU per poll.
    fn rank(&mut self, node: u32, w: u8, members: &Members, page: u64) -> Result<Vec<u32>, Error> {
        let key = page.to_be_bytes();
        let placement: &Placement<Phantom> =
            &self.nodes[node as usize].workers[w as usize].placement;
        let (owners, polls) = drive(placement.rank_async(members, &key));
        self.metrics.add("ranking_polls", polls);
        let costs = self.fabric.costs;
        self.fabric
            .cpu(self.engine.now(), node, w, polls * costs.ranking_chunk_ns);
        let owners = owners.map_err(|_| Error::Overloaded)?;
        Ok(owners
            .into_iter()
            .map(|p| members.members()[p].node)
            .collect())
    }

    fn begin_walk(&mut self, id: u64) {
        let Some(fill) = self.fills.get(&id) else {
            return;
        };
        let (node, w, page) = (fill.node, fill.worker, fill.page);
        let version = self.adopt(node);
        let members = self
            .versions
            .get(version)
            .expect("adopted version retained")
            .members
            .clone();
        let cands = match self.rank(node, w, &members, page) {
            Ok(cands) if !cands.is_empty() => cands,
            Ok(_) => return self.hard_fail(id, Error::Unavailable),
            Err(e) => return self.hard_fail(id, e),
        };
        let rank = cands.iter().position(|c| *c == node);
        let order = if rank.is_some() {
            VisitOrder::canonical(0)
        } else {
            let me = position(&members, node);
            let positions: Vec<Option<usize>> =
                cands.iter().map(|c| position(&members, *c)).collect();
            let object = self.ids.object(page);
            let number = self.ids.number(page);
            let requester = self.ids.node(node);
            visit_order(
                ReplicaSelection::Spread,
                cands.len(),
                |r| matches!((me, positions[r]), (Some(a), Some(b)) if members.is_neighbor(a, b)),
                |r| replica_score(requester, &object, number, self.ids.node(cands[r])),
            )
        };
        let fill = self.fills.get_mut(&id).expect("present");
        if *fill.plan.step() == FillStep::Hedge {
            let facts = HedgeFacts {
                page_operation: true,
                is_candidate: rank.is_some(),
                owners: cands.len(),
                first: order.get(0).unwrap_or(0),
                second: order.get(1).unwrap_or(0),
                distinct: cands.len() >= 2,
                remaining_attempts: fill.budget.remaining_attempts(),
                remaining_links: fill.budget.remaining_links(),
            };
            if hedge_eligible(facts) {
                // Hedging is evaluated but not raced; see the design doc.
                self.metrics.incr("hedge_eligible");
            }
            fill.plan.hedge(false);
        }
        fill.walk = Walking::Candidates(CandidateWalk::new(WalkConfig {
            rank,
            owners: cands.len(),
            order,
            page_operation: true,
            continuation: Continuation::default(),
        }));
        fill.cands = cands;
        fill.version = version;
        fill.rank = rank;
        self.step_walk(id);
    }

    fn step_walk(&mut self, id: u64) {
        loop {
            let Some(fill) = self.fills.get_mut(&id) else {
                return;
            };
            let Walking::Candidates(walk) = &mut fill.walk else {
                return;
            };
            match walk.next(&mut fill.budget) {
                Err(e) => return self.hard_fail(id, e),
                Ok(Step::End(end)) => return self.on_end(id, end),
                Ok(Step::Probe(probe)) => match self.send_probe(id, probe) {
                    Ok(()) | Err(None) => return,
                    Err(Some(outcome)) => {
                        let fill = self.fills.get_mut(&id).expect("present");
                        let Walking::Candidates(walk) = &mut fill.walk else {
                            return;
                        };
                        match walk.complete(&mut fill.budget, Outcome::Transport(outcome)) {
                            Err(e) => return self.hard_fail(id, e),
                            Ok(Some(end)) => return self.on_end(id, end),
                            Ok(None) => {}
                        }
                    }
                },
            }
        }
    }

    fn step_remaining(&mut self, id: u64) {
        loop {
            let Some(fill) = self.fills.get_mut(&id) else {
                return;
            };
            let Walking::Remaining(walk) = &mut fill.walk else {
                return;
            };
            match walk.next_probe() {
                Err(end) => {
                    fill.plan.remaining(Ok(end));
                    return self.continue_plan(id);
                }
                Ok(probe) => match self.send_probe(id, probe) {
                    Ok(()) | Err(None) => return,
                    Err(Some(outcome)) => {
                        let fill = self.fills.get_mut(&id).expect("present");
                        let Walking::Remaining(walk) = &mut fill.walk else {
                            return;
                        };
                        if let Some(end) =
                            walk.complete(&mut fill.budget, Outcome::Transport(outcome))
                        {
                            fill.plan.remaining(Ok(end));
                            return self.continue_plan(id);
                        }
                    }
                },
            }
        }
    }

    /// Fail the current walk step with `e` through the fill plan.
    fn hard_fail(&mut self, id: u64, e: Error) {
        let Some(fill) = self.fills.get_mut(&id) else {
            return;
        };
        match std::mem::replace(&mut fill.walk, Walking::None) {
            Walking::Candidates(mut walk) => {
                let _ = walk.abandon(&mut fill.budget);
                fill.plan.walk(WalkResult::Failed(e));
            }
            Walking::Remaining(_) => fill.plan.remaining(Err(e)),
            Walking::None => match fill.plan.step() {
                FillStep::Walk => fill.plan.walk(WalkResult::Failed(e)),
                _ => return self.finish_fill(id, Err(e)),
            },
        }
        self.continue_plan(id);
    }

    /// Plan and send one probe. `Err(Some)` is a transport outcome the caller
    /// completes; `Err(None)` means the fill already failed.
    fn send_probe(&mut self, id: u64, probe: Probe) -> Result<(), Option<ProbeOutcome>> {
        let now = self.now();
        let instant = self.instant(now);
        let attempt_timeout = Duration::from_nanos(self.config.node.attempt_timeout_ns);
        let fill = self.fills.get_mut(&id).expect("present");
        let dest = fill.cands[probe.canonical];
        let scope = fill.scope;
        let budget = match &mut fill.walk {
            Walking::Candidates(walk) => walk.request_budget(&mut fill.budget),
            _ => &mut fill.budget,
        };
        let (before_attempts, before_links) =
            (budget.remaining_attempts(), budget.remaining_links());
        let plan = plan_attempt(
            budget,
            AttemptInputs {
                now: instant,
                caller_deadline: scope,
                remaining_opportunities: probe.remaining_opportunities,
                route: probe.route,
                mode: probe.mode,
                attempt_timeout,
                failure_links: MAX_LINKS,
            },
        );
        let plan = match plan {
            Ok(plan) => plan,
            Err(e) => {
                return match transport_outcome(e) {
                    Some(outcome) => Err(Some(outcome)),
                    None => {
                        self.hard_fail(id, e);
                        Err(None)
                    }
                };
            }
        };
        self.invariants.check(
            "budget_never_grows",
            plan.links <= before_links
                && plan.links <= MAX_LINKS
                && plan.remote_attempts < before_attempts,
            now,
            Some(fill.node),
            || {
                format!(
                    "attempt plan {} links {} attempts from {before_links} links {before_attempts} attempts",
                    plan.links, plan.remote_attempts
                )
            },
        );
        let acquire = probe.mode == Mode::Acquire;
        let deadline = plan.overall.saturating_duration_since(self.base).as_nanos() as Nanos;
        let msg = Msg {
            probe: 0,
            requester: fill.node,
            at: fill.node,
            dest,
            version: fill.version,
            page: fill.page,
            acquire,
            path: vec![fill.node],
            links: plan.links,
            attempts: plan.remote_attempts,
            deadline,
            relays: Vec::new(),
        };
        let node = fill.node;
        let pid = self.fresh();
        let mid = self.fresh();
        self.fills.get_mut(&id).expect("present").probe = Some(pid);
        self.probes.insert(pid, Probing { fill: id, acquire });
        self.msgs.insert(mid, Msg { probe: pid, ..msg });
        let timeout = self.nanos(plan.attempt_end).max(now + 1);
        self.engine.at(timeout, Event::ProbeTimeout { probe: pid });
        self.metrics.incr(if acquire {
            "peer_requests_acquire"
        } else {
            "peer_requests_copy_only"
        });
        self.forward(mid, node, None);
        Ok(())
    }

    // ---- peer messages ----

    /// Route `msg` one hop from `holder`. `plan` is the relay plan at a relay;
    /// `None` at the requester.
    fn forward(&mut self, mid: u64, holder: u32, plan: Option<RelayPlan>) {
        let now = self.now();
        let m = self.msgs.get(&mid).expect("message");
        let Some(version) = self.versions.get(m.version) else {
            return self.fail_hop(mid, holder, Fault::IncompatibleMembership);
        };
        let members = version.members.clone();
        let search = match plan {
            None => racer_core::route::search_budget(&m.path, m.links, &holder),
            Some(plan) => plan.search,
        };
        let Ok((keep, links)) = search else {
            return self.fail_hop(mid, holder, Fault::HopBudgetExhausted);
        };
        let (Some(from), Some(to)) = (position(&members, holder), position(&members, m.dest))
        else {
            return self.fail_hop(mid, holder, Fault::Unavailable);
        };
        let visited: Option<Vec<usize>> = m.path[..keep]
            .iter()
            .map(|n| position(&members, *n))
            .collect();
        let Some(visited) = visited else {
            return self.fail_hop(mid, holder, Fault::Unavailable);
        };
        let instant = self.instant(now);
        let n = &self.nodes[holder as usize];
        let blocked: Vec<usize> = n
            .suspects
            .iter()
            .filter(|s| !n.links.available(s, instant))
            .filter_map(|s| position(&members, *s))
            .collect();
        let seed = m.probe.to_be_bytes();
        let query = PathQuery {
            from,
            to,
            links,
            visited: &visited,
            blocked: &blocked,
            seed: &seed,
        };
        let (route, polls) = drive(n.paths.route(&*members, query));
        let worker = (m.probe % u64::from(self.fabric.workers)) as u8;
        if polls > 1 {
            self.metrics.add("route_search_polls", polls - 1);
            let cost = (polls - 1) * 32 * self.fabric.costs.path_expand_ns_per_node;
            self.fabric.cpu(now, holder, worker, cost);
        }
        let route = match route {
            Ok(route) => route,
            Err(topology::Error::Overloaded) => {
                return self.fail_hop(mid, holder, Fault::Overloaded);
            }
            Err(_) => {
                self.metrics.incr("route_unreachable");
                return self.fail_hop(mid, holder, Fault::Unavailable);
            }
        };
        let next = members.members()[route[1]].node;
        let m = self.msgs.get_mut(&mid).expect("message");
        if let Some(plan) = plan {
            let Ok(outbound) = plan.outbound else {
                return self.fail_hop(mid, holder, Fault::HopBudgetExhausted);
            };
            let inbound = m.links;
            self.invariants.check(
                "budget_never_grows",
                outbound < inbound,
                now,
                Some(holder),
                || format!("relay forwards {outbound} links after receiving {inbound}"),
            );
            m.path.push(holder);
            m.links = outbound;
        }
        m.at = next;
        // Link: acquire the circuit, then see whether the link works.
        let n = &mut self.nodes[holder as usize];
        let Ok(probe) = n.links.acquire(&next, instant) else {
            self.metrics.incr("link_circuit_open");
            return self.fail_hop(mid, holder, Fault::Unavailable);
        };
        drop(probe);
        let site = |x: u32| self.fabric.site[x as usize];
        let up = !self.nodes[next as usize].down
            && !self.partitions.blocked(now, site(holder), site(next));
        let n = &mut self.nodes[holder as usize];
        if up {
            n.links.success(&next);
            let costs = self.fabric.costs;
            let signed = self.fabric.crypto(now, holder, costs.sign_ns);
            let arrive = self.fabric.message(signed, holder, next);
            self.metrics.incr("peer_messages");
            // The receiver verifies when the message arrives (in `hop`), so
            // no future slot is reserved on its crypto helper.
            self.engine.at(arrive, Event::Hop { msg: mid });
        } else {
            self.metrics.incr("link_failures");
            let _ = n.links.failure(&next, instant, link_backoff);
            if !n.suspects.contains(&next) && n.suspects.len() < topology::MAX_DEGREE {
                n.suspects.push(next);
            }
            self.engine.after(
                self.config.node.connect_timeout_ns,
                Event::LinkDown { msg: mid, holder },
            );
        }
    }

    /// Forwarding from `holder` failed with `fault`.
    fn fail_hop(&mut self, mid: u64, holder: u32, fault: Fault) {
        let Some(m) = self.msgs.get(&mid) else {
            return;
        };
        if holder == m.requester && m.path.len() == 1 {
            // The requester's own send failed: a transport error.
            let probe = m.probe;
            self.msgs.remove(&mid);
            self.engine.at(
                self.now(),
                Event::Reply {
                    probe,
                    reply: Reply::Transport,
                },
            );
            return;
        }
        let now = self.now();
        let reply = match relay_failure(fault) {
            RelayFailure::Respond(r) => Some(Reply::Response(response_kind(r))),
            RelayFailure::RespondIfLive(r) => {
                (now < m.deadline).then_some(Reply::Response(response_kind(r)))
            }
            RelayFailure::Propagate => Some(Reply::Transport),
        };
        match reply {
            Some(reply) => self.send_reply(mid, holder, reply, now),
            None => {
                self.metrics.incr("relay_failures_dropped");
                self.msgs.remove(&mid);
            }
        }
    }

    fn hop(&mut self, mid: u64) {
        let now = self.now();
        let Some(m) = self.msgs.get(&mid) else {
            return;
        };
        let at = m.at;
        if self.nodes[at as usize].down {
            self.metrics.incr("messages_lost_to_crash");
            self.msgs.remove(&mid);
            return;
        }
        self.fabric.crypto(now, at, self.fabric.costs.verify_ns);
        self.adopt(at);
        let m = self.msgs.get(&mid).expect("message");
        let known = self.versions.known(at, m.version, now);
        let hop = decide_hop(
            &at,
            HopRoute {
                visited: &m.path,
                destination: &m.dest,
                links: m.links,
            },
            known,
        );
        match hop {
            Hop::StaleMembership => {
                self.metrics.incr("stale_membership_replies");
                self.send_reply(mid, at, Reply::Response(ResponseKind::StaleMembership), now);
            }
            Hop::Serve => {
                let used = m.path.len();
                self.invariants.check(
                    "route_within_hop_limit",
                    used <= usize::from(MAX_LINKS),
                    now,
                    Some(at),
                    || format!("request reached its destination after {used} links"),
                );
                self.metrics.observe("request_hops", used as u64);
                self.serve(mid);
            }
            Hop::Relay(plan) => {
                let valid = !plan.invalid();
                self.invariants
                    .check("relay_route_valid", valid, now, Some(at), || {
                        "relay received a route that revisits it".to_owned()
                    });
                if !valid {
                    self.msgs.remove(&mid);
                    return;
                }
                self.metrics.incr("relays");
                let worker = (m.probe % u64::from(self.fabric.workers)) as usize;
                let quotas = &self.nodes[at as usize].workers[worker].quotas;
                match reserve(quotas, ResourceClass::Relay, 1) {
                    Ok(charge) => {
                        self.msgs
                            .get_mut(&mid)
                            .expect("message")
                            .relays
                            .push(charge);
                        self.forward(mid, at, Some(plan));
                    }
                    Err(_) => self.fail_hop(mid, at, Fault::Overloaded),
                }
            }
        }
    }

    fn serve(&mut self, mid: u64) {
        let now = self.now();
        let m = self.msgs.get(&mid).expect("message");
        let (dest, page) = (m.dest, m.page);
        let action = serve_action(ServeFacts {
            operation: OperationKind::Page,
            copy_only: !m.acquire,
            object_matches: true,
            cache_available: true,
        });
        match action {
            ServeAction::Reject(e) => {
                let reply = peer_error(e).map_or(Reply::Transport, Reply::Response);
                self.send_reply(mid, dest, reply, now);
            }
            ServeAction::Miss => {
                self.send_reply(mid, dest, Reply::Response(ResponseKind::Miss), now);
            }
            ServeAction::CopyOnly => {
                let w = self.ids.worker(page);
                let costs = self.fabric.costs;
                let t = self.fabric.cpu(now, dest, w, costs.request_ns);
                let n = &mut self.nodes[dest as usize];
                let worker = &mut n.workers[w as usize];
                if worker.memory.pages.contains_key(&page) {
                    let _ = worker.memory.touch(&page);
                    self.metrics.incr("copy_only_hits");
                    self.send_reply(mid, dest, Reply::Response(ResponseKind::Page), t);
                } else if n.disk.entries.get(&page).is_some() {
                    self.metrics.incr("copy_only_hits");
                    let read = self
                        .fabric
                        .disk_read(t, dest, record_bytes(self.config.page_bytes));
                    self.send_reply(mid, dest, Reply::Response(ResponseKind::Page), read);
                } else {
                    self.metrics.incr("copy_only_misses");
                    self.send_reply(mid, dest, Reply::Response(ResponseKind::Miss), t);
                }
            }
            ServeAction::Acquire => {
                let scope = self.instant(now + self.config.node.serve_timeout_ns);
                let route = self.instant(m.deadline);
                let (attempts, links) = (m.attempts, m.links);
                match inherited_budget(scope, route, attempts, links, m.path.len()) {
                    Err(e) => {
                        let reply = peer_error(e).map_or(Reply::Transport, Reply::Response);
                        self.send_reply(mid, dest, reply, now);
                    }
                    Ok(budget) => {
                        self.invariants.check(
                            "budget_never_grows",
                            budget.remaining_links() < links
                                && budget.remaining_attempts() <= attempts
                                && budget.deadline() <= serve_deadline(scope, route),
                            now,
                            Some(dest),
                            || {
                                format!(
                                    "inherited {} links {} attempts from {links} links {attempts} attempts",
                                    budget.remaining_links(),
                                    budget.remaining_attempts()
                                )
                            },
                        );
                        self.metrics.incr("peer_acquires");
                        self.want(dest, page, Want::Peer { msg: mid }, budget, false);
                    }
                }
            }
        }
    }

    /// Send `reply` for `mid` from `from` back along the reverse route,
    /// starting at `ready`. Pages travel as cut-through transfers.
    fn send_reply(&mut self, mid: u64, from: u32, reply: Reply, ready: Nanos) {
        let Some(m) = self.msgs.remove(&mid) else {
            return;
        };
        let now = self.now();
        let mut route: Vec<u32> = Vec::with_capacity(m.path.len() + 1);
        route.push(from);
        route.extend(m.path.iter().rev().copied());
        let site = |x: u32| self.fabric.site[x as usize];
        let alive = route.windows(2).all(|hop| {
            !self.nodes[hop[1] as usize].down
                && !self.partitions.blocked(now, site(hop[0]), site(hop[1]))
        });
        if !alive {
            self.metrics.incr("replies_lost");
            return;
        }
        let costs = self.fabric.costs;
        let links = (route.len() - 1) as u64;
        let arrive = if reply == Reply::Response(ResponseKind::Page) {
            let bytes = self.config.page_bytes;
            let sealed = self.fabric.crypto(ready, from, costs.sign_ns);
            let done = self.fabric.transfer(sealed, &route, bytes);
            self.metrics.add("peer_bytes", bytes * links);
            self.metrics.observe("hops", links);
            done
        } else {
            let mut t = self.fabric.crypto(ready, from, costs.sign_ns);
            for hop in route.windows(2) {
                t = self.fabric.message(t, hop[0], hop[1]);
            }
            t
        };
        self.engine.at(
            arrive,
            Event::Reply {
                probe: m.probe,
                reply,
            },
        );
    }

    fn reply(&mut self, pid: u64, reply: Reply) {
        let Some(probe) = self.probes.remove(&pid) else {
            self.metrics.incr("late_replies");
            return;
        };
        let Some(fill) = self.fills.get(&probe.fill) else {
            return;
        };
        let now = self.now();
        self.fabric
            .crypto(now, fill.node, self.fabric.costs.verify_ns);
        let page = fill.page;
        let outcome = match reply {
            Reply::Transport => Ok(Outcome::Transport(ProbeOutcome::Unreachable)),
            Reply::Response(ResponseKind::StaleMembership) => Ok(Outcome::StaleMembership),
            Reply::Response(kind) => {
                // A phantom page claims the page it was asked for; the
                // identity check is the same callback production runs.
                let expected = self.ids.page(page);
                let claimed = self.ids.page(page);
                classify(kind, OperationKind::Page, probe.acquire, || {
                    if claimed == expected {
                        Ok(())
                    } else {
                        Err(Error::CorruptRecord)
                    }
                })
                .map(|outcome| match outcome {
                    None => Outcome::Copy,
                    Some(outcome) => Outcome::Probe(outcome),
                })
            }
        };
        self.complete_probe(probe.fill, outcome);
    }

    fn complete_probe(&mut self, id: u64, outcome: Result<Outcome, Error>) {
        let Some(fill) = self.fills.get_mut(&id) else {
            return;
        };
        fill.probe = None;
        match &mut fill.walk {
            Walking::Candidates(walk) => match outcome {
                Err(e) => self.hard_fail(id, e),
                Ok(outcome) => match walk.complete(&mut fill.budget, outcome) {
                    Err(e) => self.hard_fail(id, e),
                    Ok(Some(end)) => self.on_end(id, end),
                    Ok(None) => self.step_walk(id),
                },
            },
            Walking::Remaining(walk) => match outcome {
                Err(e) => self.hard_fail(id, e),
                Ok(outcome) => match walk.complete(&mut fill.budget, outcome) {
                    Some(end) => {
                        fill.plan.remaining(Ok(end));
                        self.continue_plan(id);
                    }
                    None => self.step_remaining(id),
                },
            },
            Walking::None => {}
        }
    }

    fn on_end(&mut self, id: u64, end: End) {
        let fill = self.fills.get_mut(&id).expect("present");
        fill.walk = Walking::None;
        match end {
            End::Copy => {
                self.metrics.incr("walk_copies");
                fill.plan.walk(WalkResult::Copy);
            }
            End::StaleMembership => {
                let (node, retried, previous) = (fill.node, fill.retried, fill.version);
                let latest = self.adopt(node);
                match refresh_allowed(retried, previous, Some(latest)) {
                    Ok(()) => {
                        self.metrics.incr("walk_membership_refreshes");
                        self.fills.get_mut(&id).expect("present").retried = true;
                        return self.begin_walk(id);
                    }
                    Err(e) => {
                        let fill = self.fills.get_mut(&id).expect("present");
                        fill.plan.walk(WalkResult::Failed(e));
                    }
                }
            }
            End::Origin { rank, evidence } => return self.origin_authority(id, rank, evidence),
            End::VersionUnavailable => fill
                .plan
                .walk(WalkResult::Failed(Error::VersionUnavailable)),
            End::Exhausted { funding } => {
                self.metrics.incr(if funding {
                    "walk_exhausted_funding"
                } else {
                    "walk_exhausted"
                });
                fill.plan.walk(WalkResult::Failed(Error::Unavailable));
            }
        }
        self.continue_plan(id);
    }

    /// Recompute the candidates before origin (production
    /// `resolve_epoch_traced`), validate origin authority, and check that only
    /// a ranked owner reaches the origin.
    fn origin_authority(&mut self, id: u64, rank: usize, evidence: Vec<ProbeOutcome>) {
        let now = self.now();
        let fill = self.fills.get(&id).expect("present");
        let (node, w, page, version) = (fill.node, fill.worker, fill.page, fill.version);
        let cands = fill.cands.clone();
        let Some(members) = self.versions.get(version).map(|v| v.members.clone()) else {
            return self.fail_walk(id, Error::IncompatibleMembership);
        };
        let expected = match self.rank(node, w, &members, page) {
            Ok(expected) => expected,
            Err(e) => return self.fail_walk(id, e),
        };
        if expected != cands {
            return self.fail_walk(id, Error::IncompatibleMembership);
        }
        let object = self.ids.object(page);
        let number = self.ids.number(page);
        let authority = validate_origin_authority(
            AuthorityFacts {
                object: &object,
                page: number,
                rank,
                predecessor_evidence: evidence.len(),
                member: position(&members, node).is_some(),
            },
            &object,
            number,
        );
        let owner = cands.get(rank) == Some(&node);
        let ok = owner && authority.is_ok();
        self.invariants
            .check("only_owner_hits_origin", ok, now, Some(node), || {
                format!("page {page} rank {rank} owners {cands:?} authority {authority:?}")
            });
        if !ok {
            return self.fail_walk(id, Error::Unauthorized);
        }
        if version < self.versions.latest().number {
            self.metrics.incr("origin_on_older_membership");
        }
        let fill = self.fills.get_mut(&id).expect("present");
        fill.plan.walk(WalkResult::Origin { evidence });
        self.issue_origin(id);
    }

    fn fail_walk(&mut self, id: u64, e: Error) {
        let fill = self.fills.get_mut(&id).expect("present");
        fill.plan.walk(WalkResult::Failed(e));
        self.continue_plan(id);
    }

    fn issue_origin(&mut self, id: u64) {
        let now = self.now();
        let page = self.fills[&id].page;
        let errors = origin_errors_at(&self.config.faults, self.config.origin_errors, now);
        let object = page / u64::from(self.config.pages_per_object);
        let (done, outcome) = self
            .origin
            .request(now, object, self.config.page_bytes, errors);
        self.metrics.incr("origin_requests");
        *self.origin_per_page.entry(page).or_default() += 1;
        self.engine
            .at(done, Event::OriginDone { fill: id, outcome });
    }

    fn origin_done(&mut self, id: u64, outcome: OriginOutcome) {
        let Some(fill) = self.fills.get_mut(&id) else {
            return;
        };
        let result = match outcome {
            OriginOutcome::Ok => {
                self.metrics.add("origin_bytes", self.config.page_bytes);
                Ok(())
            }
            OriginOutcome::PreconditionFailed => {
                self.metrics.incr("origin_412");
                Err(Error::VersionUnavailable)
            }
            OriginOutcome::Forbidden => {
                self.metrics.incr("origin_403");
                Err(Error::OriginForbidden)
            }
            OriginOutcome::ServerError => {
                self.metrics.incr("origin_5xx");
                Err(Error::Unavailable)
            }
        };
        fill.plan.origin(result);
        self.continue_plan(id);
    }

    fn continue_plan(&mut self, id: u64) {
        let Some(fill) = self.fills.get_mut(&id) else {
            return;
        };
        match fill.plan.step().clone() {
            FillStep::Publish(source) => self.publish_page(id, source),
            FillStep::RemainingCopy => {
                self.metrics.incr("remaining_walks");
                let rank = fill.rank.unwrap_or(0);
                fill.walk = Walking::Remaining(RemainingWalk::new(rank, fill.cands.len()));
                self.step_remaining(id);
            }
            FillStep::Failed(e) => self.finish_fill(id, Err(e)),
            FillStep::Done(source) => self.finish_fill(id, Ok(source)),
            step => {
                self.metrics.incr("fill_plan_stalled");
                debug_assert!(false, "fill plan stalled at {step:?}");
                self.finish_fill(id, Err(Error::Internal));
            }
        }
    }

    fn publish_page(&mut self, id: u64, source: Source) {
        let now = self.now();
        let instant = self.instant(now);
        let fill = self.fills.get(&id).expect("present");
        let (node, w, page, owned) = (fill.node, fill.worker, fill.page, fill.rank.is_some());
        self.memory_insert(node, w, page);
        let n = &mut self.nodes[node as usize];
        let seen = n.retention.history.observe(&page, instant);
        n.retention.heat.touch(&page, instant);
        let quotas = &n.workers[w as usize].quotas;
        let decision = persist(PersistFacts {
            eligible: retention_eligible(owned, true, seen),
            metadata_valid: true,
            resident: n.disk.entries.get(&page).is_some(),
            owned,
            dirty_limit: quotas.limit(ResourceClass::DirtyCiphertext),
            dirty_used: quotas.used(ResourceClass::DirtyCiphertext),
            amount: PAGE_BYTES + 16,
            one_page: PAGE_BYTES,
        });
        self.metrics.incr(match decision {
            Persist::Ineligible => "persist_ineligible",
            Persist::Resident => "persist_resident",
            Persist::NoHeadroom => "persist_no_headroom",
            Persist::Write => "persist_write",
        });
        if decision == Persist::Write {
            self.disk_write(node, w, page);
        }
        let fill = self.fills.get_mut(&id).expect("present");
        fill.plan.published(Ok(()));
        let _ = source;
        self.continue_plan(id);
    }

    fn disk_write(&mut self, node: u32, w: u8, page: u64) {
        let now = self.now();
        let instant = self.instant(now);
        let page_bytes = self.config.page_bytes;
        let record = record_bytes(page_bytes);
        let version = self.nodes[node as usize].version;
        let members = self.versions.get(version).map(|v| v.members.clone());
        let me = members.as_ref().and_then(|m| position(m, node));
        let n = &mut self.nodes[node as usize];
        let charge = match reserve(
            &n.workers[w as usize].quotas,
            ResourceClass::DirtyCiphertext,
            PAGE_BYTES + 16,
        ) {
            Ok(charge) => charge,
            Err(_) => {
                self.metrics.incr("disk_write_rejected");
                return;
            }
        };
        let mut appended = n.disk.segments.append(record as usize);
        if appended.is_err() {
            let Node {
                workers,
                disk,
                retention,
                ..
            } = n;
            let ids = &self.ids;
            let owned = |p: &u64| match (&members, me) {
                (Some(m), Some(me)) => workers[ids.worker(*p) as usize].placement.is_owner(
                    &**m,
                    &p.to_be_bytes(),
                    |o| o == me,
                ),
                _ => false,
            };
            let _ = disk
                .clock
                .reclaim_scored(&disk.entries, 1, 64, 256, |segment| {
                    disk.entries.segment_score(
                        segment,
                        SCAN_BUDGET,
                        unseen_value(page_bytes),
                        |p, bytes| {
                            let heat = retention.heat.score(p, instant);
                            page_value(
                                Some(bytes),
                                page_bytes as u32,
                                retention_score(heat, owned(p), true),
                            )
                        },
                    )
                });
            let evicted = disk.entries.take_evicted();
            for (p, _) in &evicted {
                retention.heat.forget(p);
            }
            self.metrics.add("disk_evictions", evicted.len() as u64);
            appended = disk.segments.append(record as usize);
        }
        let n = &mut self.nodes[node as usize];
        let Ok((lease, extent)) = appended else {
            self.metrics.incr("disk_write_skipped");
            return;
        };
        let segment = SegmentId(extent.offset() / n.disk.segment_bytes());
        drop(lease);
        if n.disk
            .entries
            .publish(page, page_bytes as u32, segment)
            .is_err()
        {
            self.metrics.incr("disk_write_skipped");
            return;
        }
        n.retention.heat.track(&page, instant);
        self.metrics.incr("disk_writes");
        let done = self.fabric.disk_write(now, node, record);
        let write = self.fresh();
        self.writes.insert(write, charge);
        self.engine.at(done, Event::Written { write });
    }

    fn finish_fill(&mut self, id: u64, result: Result<Source, Error>) {
        let Some(fill) = self.fills.remove(&id) else {
            return;
        };
        if let Some(probe) = fill.probe {
            self.probes.remove(&probe);
        }
        let now = self.now();
        let n = &mut self.nodes[fill.node as usize];
        if n.incarnation != fill.incarnation {
            return;
        }
        self.metrics.incr(match result {
            Ok(Source::Origin) => "fills_from_origin",
            Ok(Source::Peer) => "fills_from_peer",
            Ok(Source::Local) => "fills_local",
            Ok(Source::Hedge) => "fills_from_hedge",
            Err(_) => "fills_failed",
        });
        let page_id = self.ids.page(fill.page);
        let outcome = match result {
            Ok(_) => FlightOutcome::Published(self.config.page_bytes as u32),
            Err(e) => FlightOutcome::Failed(e),
        };
        let mut wakes = Vec::new();
        let worker = &mut n.workers[fill.worker as usize];
        let finished = worker
            .flights
            .finish(&page_id, fill.leader, outcome, &mut wakes);
        self.invariants.check(
            "flight_leader_valid",
            finished.is_ok(),
            now,
            Some(fill.node),
            || {
                format!(
                    "finish page {} leader {}: {finished:?}",
                    fill.page, fill.leader
                )
            },
        );
        let leader = self.tokens.remove(&fill.token);
        self.detach(fill.node, fill.worker, fill.page, fill.leader);
        if let Some(leader) = leader {
            let got = result.map(|source| match source {
                Source::Origin => Got::Origin,
                Source::Peer | Source::Hedge => Got::Peer,
                Source::Local => Got::Local,
            });
            self.resolve(leader.want, got);
        }
        for token in wakes {
            if token != fill.token {
                self.wake(token);
            }
        }
    }

    // ---- report ----

    pub fn memory(&self) -> Vec<(&'static str, usize)> {
        let mut components = Components::default();
        for n in &self.nodes {
            n.account(&mut components);
        }
        let mut list = components.list();
        // Shared memos: real rankings (three scores and a cursor) and route
        // alternatives, estimated per entry.
        list.push(("placement_memo", self.shared.phantom.memo_len() * 192));
        list.push(("route_memo", self.shared.routes.memo_len() * 320));
        list.push(("membership_versions", self.versions.approximate_bytes()));
        list.push(("engine", self.engine.approximate_bytes()));
        list.push(("fabric", self.fabric.approximate_bytes()));
        list.push(("origin", self.origin.approximate_bytes()));
        list.push(("ids", self.ids.approximate_bytes()));
        list.push(("workload", self.reads.approximate_bytes()));
        list.push((
            "in_flight",
            self.fills.len() * (std::mem::size_of::<Fill>() + 64)
                + self.tokens.len() * (std::mem::size_of::<Waiting>() + 16)
                + self.msgs.len() * (std::mem::size_of::<Msg>() + 64)
                + self.probes.len() * 32
                + self.ranges.len() * 96
                + self.writes.len() * 96,
        ));
        list.push(("origin_per_page", self.origin_per_page.capacity() * 16));
        list.push((
            "node_rng",
            self.node_rng.capacity() * std::mem::size_of::<SplitMix64>(),
        ));
        list
    }

    pub fn rejections(&self) -> Value {
        let counts = *self.shared.rejections.borrow();
        let mut out = serde_json::Map::new();
        for class in ResourceClass::ALL {
            out.insert(class.name().into(), json!(counts[class as usize]));
        }
        out.insert("keys".into(), json!(counts[ResourceClass::ALL.len()]));
        Value::Object(out)
    }

    pub fn report(&self, scenario: &str, seed: u64) -> Value {
        let memory = self.memory();
        let total: usize = memory.iter().map(|(_, b)| b).sum();
        let nodes = self.config.nodes.max(1) as usize;
        let duplicated = self.origin_per_page.values().filter(|c| **c > 1).count();
        let max_per_page = self.origin_per_page.values().copied().max().unwrap_or(0);
        let requests: u64 = self.origin_per_page.values().map(|c| u64::from(*c)).sum();
        let pages = self.origin_per_page.len();
        let c = |k: &str| self.metrics.get(k);
        json!({
            "scenario": scenario,
            "logic": "real",
            "nodes": self.config.nodes,
            "seed": seed,
            "duration_ns": self.engine.now(),
            "events": self.engine.processed(),
            "trace_hash": format!("{:016x}", self.engine.trace.value()),
            "metrics": self.metrics.to_json(),
            "origin_per_page": {
                "pages": pages,
                "requests": requests,
                "pages_fetched_more_than_once": duplicated,
                "max_requests_per_page": max_per_page,
                "mean_requests_per_page": if pages == 0 { 0.0 } else { requests as f64 / pages as f64 },
                "page_bytes": self.config.page_bytes,
                "bytes_per_page": if pages == 0 { 0 } else { c("origin_bytes") / pages as u64 },
            },
            "cache": {
                "memory_hits": c("memory_hits"),
                "disk_hits": c("disk_hits"),
                "peer_memory_hits": c("peer_memory_hits"),
                "peer_disk_hits": c("peer_disk_hits"),
                "copy_only_hits": c("copy_only_hits"),
                "local_misses": c("local_misses"),
                "memory_evictions": c("memory_evictions"),
                "disk_evictions": c("disk_evictions"),
                "flight_joins": c("flight_joins"),
            },
            "peer": {
                "bytes": c("peer_bytes"),
                "messages": c("peer_messages"),
                "relays": c("relays"),
            },
            "rejections": self.rejections(),
            "membership": {
                "latest_version": self.versions.latest().number,
                "publications": c("membership_publications"),
                "placement_memo_entries": self.shared.phantom.memo_len(),
                "route_memo_entries": self.shared.routes.memo_len(),
            },
            "invariants": self.invariants.to_json(),
            "memory": {
                "components": memory.iter().map(|(k, v)| ((*k).to_owned(), json!(v))).collect::<serde_json::Map<_, _>>(),
                "accounted_bytes": total,
                "accounted_bytes_per_node": total / nodes,
                "peak_rss_bytes": peak_rss_bytes(),
            },
        })
    }

    /// Origin requests for `page` so far.
    pub fn origin_requests(&self, page: u64) -> u32 {
        self.origin_per_page.get(&page).copied().unwrap_or(0)
    }

    /// Shared handles for tests.
    pub fn members(&self) -> Rc<Members> {
        self.versions.latest().members.clone()
    }
}
