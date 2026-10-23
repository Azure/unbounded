//! The simulated world and the placeholder "caches-only" read flow.
//!
//! [`World`] owns the engine, the fabric, the origin, membership, a
//! [`NodeLogic`] and all metrics. The read flow here is a fixed stand-in:
//!
//! 1. A read arrives at a node and is dispatched to a worker CPU.
//! 2. Memory hit: done. Disk hit: disk read plus AEAD open, then done.
//! 3. Miss: ask the owner over a route (sign per message, verify per hop).
//!    A dead or unreachable owner makes the requester retry after a timeout.
//! 4. The owner serves from its tiers or joins/starts a per-page flight to
//!    the origin (singleflight), then streams the page back cut-through.
//!
//! Real decision logic replaces steps 3 and 4 (see [`crate::logic`]).

use std::collections::HashMap;
use std::hash::BuildHasherDefault;

use racer_core::time::SplitMix64;
use serde_json::{Value, json};

use crate::config::Config;
use crate::engine::{Engine, MILLIS, Nanos, Role, Streams};
use crate::faults::{Fault, Partitions, origin_errors_at};
use crate::logic::{Membership, NodeLogic, Tier, View};
use crate::report::{Invariants, MemoryUsage, Metrics, peak_rss_bytes};
use crate::resources::{Fabric, Origin, OriginOutcome};
use crate::workload::{Poisson, Reads, page_id};

/// Deterministic hasher for simulator maps (fixed-key SipHash).
pub type Fixed = BuildHasherDefault<std::collections::hash_map::DefaultHasher>;

/// Maximum read attempts before a read fails.
pub const MAX_ATTEMPTS: u8 = 4;

#[derive(Clone, Copy, Debug)]
pub struct Waiter {
    pub node: u32,
    pub start: Nanos,
    pub attempt: u8,
}

#[derive(Clone, Copy, Debug)]
pub enum Event {
    /// Poisson arrival on a node; draws a read and schedules the next one.
    Arrive {
        node: u32,
    },
    Read(Waiter, u64),
    /// Request reached `owner` after `links` links with `budget` remaining.
    AtOwner {
        owner: u32,
        waiter: Waiter,
        page: u64,
        links: u8,
        budget: u8,
    },
    OriginDone {
        page: u64,
        flight: u64,
        outcome: OriginOutcome,
    },
    /// Re-issue a flight's origin request after a 5xx backoff.
    OriginRetry {
        page: u64,
        flight: u64,
    },
    Delivered {
        waiter: Waiter,
        page: u64,
        links: u8,
    },
    Crash {
        node: u32,
    },
    Restart {
        node: u32,
    },
    Membership {
        node: u32,
        live: bool,
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
            Self::Read(w, p) => (2, w.node.into(), p),
            Self::AtOwner { owner, page, .. } => (3, owner.into(), page),
            Self::OriginDone { page, flight, .. } => (4, flight, page),
            Self::Delivered { waiter, page, .. } => (5, waiter.node.into(), page),
            Self::Crash { node } => (6, node.into(), 0),
            Self::Restart { node } => (7, node.into(), 0),
            Self::Membership { node, live } => (8, node.into(), live.into()),
            Self::Slow { node, factor } => (9, node.into(), factor.into()),
            Self::OriginRetry { page, flight } => (10, flight, page),
        }
    }
}

struct Flight {
    id: u64,
    owner: u32,
    attempt: u8,
    waiters: Vec<Waiter>,
}

pub struct World<L: NodeLogic> {
    pub config: Config,
    pub engine: Engine<Event>,
    pub fabric: Fabric,
    pub origin: Origin,
    pub membership: Membership,
    pub logic: L,
    pub metrics: Metrics,
    pub invariants: Invariants,
    partitions: Partitions,
    /// Crashed nodes (process down). Distinct from membership, which lags.
    down: Vec<bool>,
    node_rng: Vec<SplitMix64>,
    reads: Reads,
    arrivals: Poisson,
    flights: HashMap<u64, Flight, Fixed>,
    next_flight: u64,
    origin_per_page: HashMap<u64, u32, Fixed>,
    route: Vec<u32>,
    last_version: u64,
}

impl<L: NodeLogic> World<L> {
    pub fn new(config: Config, seed: u64, logic: L) -> Self {
        let streams = Streams::new(seed);
        let fabric = Fabric::new(&config, &mut streams.stream(Role::Topology, 0));
        let origin = Origin::new(&config, streams.stream(Role::Origin, 0));
        let n = config.nodes;
        let mut world = Self {
            engine: Engine::new(),
            fabric,
            origin,
            membership: Membership::all(n),
            logic,
            metrics: Metrics::default(),
            invariants: Invariants::default(),
            partitions: Partitions::from_faults(&config.faults),
            down: vec![false; n as usize],
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
            flights: HashMap::default(),
            next_flight: 0,
            origin_per_page: HashMap::default(),
            route: Vec::with_capacity(16),
            last_version: 0,
            config,
        };
        world.last_version = world.membership.version();
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
                    let w = Waiter {
                        node,
                        start: at,
                        attempt: 0,
                    };
                    self.engine.at(at, Event::Read(w, page));
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
                Fault::Crash {
                    node,
                    at_ns,
                    restart_ns,
                } => {
                    self.engine.at(at_ns, Event::Crash { node });
                    self.engine
                        .at(restart_ns.max(at_ns), Event::Restart { node });
                }
                Fault::Slow {
                    node,
                    factor,
                    from_ns,
                    until_ns,
                } => {
                    self.engine.at(from_ns, Event::Slow { node, factor });
                    self.engine
                        .at(until_ns.max(from_ns), Event::Slow { node, factor: 1 });
                }
                Fault::Partition { .. } | Fault::OriginErrors { .. } => {}
            }
        }
    }

    fn valid_node(&self, node: u32) -> bool {
        node < self.config.nodes
    }

    /// Run until `end` (or until the queue drains).
    pub fn run(&mut self, end: Nanos) {
        while let Some(event) = self.engine.pop_until(end) {
            let now = self.engine.now();
            let (tag, a, b) = event.tag();
            self.engine.trace.record(now, tag, a, b);
            self.handle(event);
        }
        self.engine.finish(end);
    }

    fn handle(&mut self, event: Event) {
        let now = self.engine.now();
        match event {
            Event::Arrive { node } => {
                if !self.valid_node(node) {
                    return;
                }
                let rng = &mut self.node_rng[node as usize];
                if let Some(gap) = self.arrivals.gap(rng) {
                    self.engine.after(gap, Event::Arrive { node });
                }
                if self.down[node as usize] {
                    return;
                }
                let read = self.reads.draw(rng);
                if read.pages > 1 {
                    self.metrics.incr("range_reads");
                }
                for p in read.first..read.first + read.pages {
                    let page = page_id(read.object, p, self.config.pages_per_object);
                    let w = Waiter {
                        node,
                        start: now,
                        attempt: 0,
                    };
                    self.engine.at(now, Event::Read(w, page));
                }
            }
            Event::Read(w, page) => self.read(w, page),
            Event::AtOwner {
                owner,
                waiter,
                page,
                links,
                budget,
            } => self.at_owner(owner, waiter, page, links, budget),
            Event::OriginDone {
                page,
                flight,
                outcome,
            } => self.origin_done(page, flight, outcome),
            Event::OriginRetry { page, flight } => {
                if self.flights.get(&page).is_some_and(|f| f.id == flight) {
                    self.issue_origin(page, flight);
                }
            }
            Event::Delivered {
                waiter,
                page,
                links,
            } => {
                if self.down[waiter.node as usize] {
                    self.metrics.incr("deliveries_dropped");
                    return;
                }
                if self.logic.insert(waiter.node, page, Tier::Memory) {
                    self.metrics.incr("memory_evictions");
                }
                self.metrics.observe("hops", links.into());
                self.complete(waiter, "read_remote_ns");
            }
            Event::Crash { node } => {
                if !self.valid_node(node) || self.down[node as usize] {
                    return;
                }
                self.down[node as usize] = true;
                self.metrics.incr("crashes");
                self.logic.crash(node);
                // Flights owned by the crashed node die; waiters retry.
                let mut orphans = Vec::new();
                let mut dead: Vec<u64> = self
                    .flights
                    .iter()
                    .filter(|(_, f)| f.owner == node)
                    .map(|(p, _)| *p)
                    .collect();
                dead.sort_unstable();
                for page in dead {
                    let f = self.flights.remove(&page).expect("present");
                    orphans.extend(f.waiters.into_iter().map(|w| (w, page)));
                }
                for (w, page) in orphans {
                    self.retry(w, page, "flight_lost");
                }
            }
            Event::Restart { node } => {
                if self.valid_node(node) && self.down[node as usize] {
                    self.down[node as usize] = false;
                    self.fabric.reset_node(node, now);
                    self.metrics.incr("restarts");
                }
            }
            Event::Membership { node, live } => {
                if self.membership.set(node, live) {
                    self.metrics.incr("membership_changes");
                }
                let v = self.membership.version();
                let last = self.last_version;
                self.invariants.check(
                    "membership_version_monotonic",
                    v >= last,
                    now,
                    Some(node),
                    || format!("version {v} after {last}"),
                );
                self.last_version = v;
            }
            Event::Slow { node, factor } => {
                if self.valid_node(node) {
                    self.fabric.slow[node as usize] = factor.max(1);
                }
            }
        }
    }

    fn worker(&self, page: u64) -> u8 {
        (crate::engine::mix(page) % u64::from(self.fabric.workers)) as u8
    }

    fn complete(&mut self, w: Waiter, histogram: &'static str) {
        let now = self.engine.now();
        self.metrics.incr("reads_completed");
        self.metrics.observe(histogram, now - w.start);
        self.metrics.observe("read_ns", now - w.start);
    }

    fn fail(&mut self, _w: Waiter, class: &'static str) {
        self.metrics.incr("reads_failed");
        self.metrics.incr(class);
    }

    fn retry(&mut self, mut w: Waiter, page: u64, class: &'static str) {
        self.metrics.incr(class);
        w.attempt += 1;
        if w.attempt >= MAX_ATTEMPTS {
            self.fail(w, "rejected_attempts");
            return;
        }
        let backoff = 2 * self.fabric.rtt.rtt(0, 0).max(MILLIS) * u64::from(w.attempt);
        self.engine.after(backoff, Event::Read(w, page));
    }

    fn read(&mut self, w: Waiter, page: u64) {
        let node = w.node;
        if !self.valid_node(node) || self.down[node as usize] {
            return;
        }
        let now = self.engine.now();
        let costs = self.fabric.costs;
        let cpu_done = self
            .fabric
            .cpu(now, node, self.worker(page), costs.request_ns);
        match self.logic.lookup(node, page) {
            Tier::Memory => {
                self.metrics.incr("memory_hits");
                self.engine.at(
                    cpu_done,
                    Event::Delivered {
                        waiter: w,
                        page,
                        links: 0,
                    },
                );
            }
            Tier::Disk => {
                self.metrics.incr("disk_hits");
                let read = self
                    .fabric
                    .disk_read(cpu_done, node, self.config.page_bytes);
                let open = self
                    .fabric
                    .crypto(read, node, costs.aead(self.config.page_bytes));
                self.engine.at(
                    open,
                    Event::Delivered {
                        waiter: w,
                        page,
                        links: 0,
                    },
                );
            }
            Tier::Miss => {
                self.metrics.incr("local_misses");
                let Some(owner) = self.logic.owner(&self.membership, page) else {
                    self.fail(w, "rejected_no_members");
                    return;
                };
                if owner == node {
                    self.at_owner(owner, w, page, 0, self.config.hop_limit);
                    return;
                }
                let mut route = std::mem::take(&mut self.route);
                let ok = self.logic.route(
                    &View {
                        fabric: &self.fabric,
                        partitions: &self.partitions,
                        now,
                        membership: &self.membership,
                    },
                    node,
                    owner,
                    &mut route,
                );
                if !ok {
                    self.route = route;
                    self.retry(w, page, "rejected_unreachable");
                    return;
                }
                let links = (route.len() - 1) as u8;
                let limit = self.config.hop_limit;
                self.invariants.check(
                    "route_within_hop_limit",
                    links <= limit,
                    now,
                    Some(node),
                    || format!("route {route:?} has {links} links, limit {limit}"),
                );
                // Sign once, verify at each hop; message latency per link.
                let mut t = self.fabric.crypto(cpu_done, node, costs.sign_ns);
                let mut budget = limit;
                for hop in route.windows(2) {
                    t = self.fabric.message(t, hop[0], hop[1]);
                    t = self.fabric.crypto(t, hop[1], costs.verify_ns);
                    let next = budget.saturating_sub(1);
                    self.invariants.check(
                        "budget_never_grows",
                        next <= budget,
                        now,
                        Some(hop[1]),
                        || format!("budget {budget} -> {next}"),
                    );
                    budget = next;
                }
                self.metrics.add("peer_requests", 1);
                self.route = route;
                self.engine.at(
                    t,
                    Event::AtOwner {
                        owner,
                        waiter: w,
                        page,
                        links,
                        budget,
                    },
                );
            }
        }
    }

    fn at_owner(&mut self, owner: u32, w: Waiter, page: u64, links: u8, budget: u8) {
        let _ = budget;
        let now = self.engine.now();
        if self.down[owner as usize] {
            self.retry(w, page, "rejected_owner_down");
            return;
        }
        let tier = self.logic.lookup(owner, page);
        if tier != Tier::Miss {
            self.metrics.incr(if tier == Tier::Memory {
                "owner_memory_hits"
            } else {
                "owner_disk_hits"
            });
            let ready = if tier == Tier::Disk {
                self.fabric.disk_read(now, owner, self.config.page_bytes)
            } else {
                now
            };
            self.serve(owner, w, page, links, ready);
            return;
        }
        if let Some(f) = self.flights.get_mut(&page) {
            f.waiters.push(w);
            self.metrics.incr("flight_joins");
            return;
        }
        let expected = self.logic.owner(&self.membership, page);
        if expected != Some(owner) {
            // Membership changed while the request was in flight: the
            // requester's view was stale, so it retries with the new view.
            self.retry(w, page, "rejected_stale_owner");
            return;
        }
        self.invariants.check(
            "only_owner_hits_origin",
            expected == Some(owner),
            now,
            Some(owner),
            || format!("page {page} fetched by {owner}, ranked owner {expected:?}"),
        );
        self.next_flight += 1;
        let id = self.next_flight;
        self.flights.insert(
            page,
            Flight {
                id,
                owner,
                attempt: 0,
                waiters: vec![w],
            },
        );
        self.issue_origin(page, id);
    }

    fn issue_origin(&mut self, page: u64, flight: u64) {
        let now = self.engine.now();
        let errors = origin_errors_at(&self.config.faults, self.config.origin_errors, now);
        let object = page / u64::from(self.config.pages_per_object);
        let (done, outcome) = self
            .origin
            .request(now, object, self.config.page_bytes, errors);
        self.metrics.incr("origin_requests");
        *self.origin_per_page.entry(page).or_default() += 1;
        self.engine.at(
            done,
            Event::OriginDone {
                page,
                flight,
                outcome,
            },
        );
    }

    fn origin_done(&mut self, page: u64, flight: u64, outcome: OriginOutcome) {
        let Some(f) = self.flights.get_mut(&page) else {
            return;
        };
        if f.id != flight {
            return;
        }
        match outcome {
            OriginOutcome::Ok => {
                let f = self.flights.remove(&page).expect("present");
                let now = self.engine.now();
                self.metrics.add("origin_bytes", self.config.page_bytes);
                if self.logic.insert(f.owner, page, Tier::Memory) {
                    self.metrics.incr("memory_evictions");
                }
                let costs = self.fabric.costs;
                let sealed = self
                    .fabric
                    .crypto(now, f.owner, costs.aead(self.config.page_bytes));
                let _ = self
                    .fabric
                    .disk_write(sealed, f.owner, self.config.page_bytes);
                if self.logic.insert(f.owner, page, Tier::Disk) {
                    self.metrics.incr("disk_evictions");
                }
                for w in f.waiters {
                    self.serve(f.owner, w, page, 0, now);
                }
            }
            OriginOutcome::ServerError | OriginOutcome::PreconditionFailed => {
                self.metrics.incr(if outcome == OriginOutcome::ServerError {
                    "origin_5xx"
                } else {
                    "origin_412"
                });
                f.attempt += 1;
                if f.attempt < MAX_ATTEMPTS {
                    // 412 refreshes metadata and retries now; 5xx backs off.
                    let delay = if outcome == OriginOutcome::ServerError {
                        self.config.origin_retry_ns
                    } else {
                        0
                    };
                    let id = f.id;
                    if delay == 0 {
                        self.issue_origin(page, id);
                    } else {
                        self.engine
                            .after(delay, Event::OriginRetry { page, flight: id });
                    }
                } else {
                    let f = self.flights.remove(&page).expect("present");
                    for w in f.waiters {
                        self.fail(w, "rejected_origin_unavailable");
                    }
                }
            }
            OriginOutcome::Forbidden => {
                self.metrics.incr("origin_403");
                let f = self.flights.remove(&page).expect("present");
                for w in f.waiters {
                    self.fail(w, "rejected_forbidden");
                }
            }
        }
    }

    /// Send `page` from `owner` to the waiter, starting at `ready`.
    fn serve(&mut self, owner: u32, w: Waiter, page: u64, links: u8, ready: Nanos) {
        if w.node == owner {
            self.engine.at(
                ready,
                Event::Delivered {
                    waiter: w,
                    page,
                    links: 0,
                },
            );
            return;
        }
        let now = self.engine.now();
        let mut route = std::mem::take(&mut self.route);
        if !self.logic.route(
            &View {
                fabric: &self.fabric,
                partitions: &self.partitions,
                now,
                membership: &self.membership,
            },
            owner,
            w.node,
            &mut route,
        ) {
            self.route = route;
            self.retry(w, page, "rejected_unreachable");
            return;
        }
        let bytes = self.config.page_bytes;
        let costs = self.fabric.costs;
        let sealed = self.fabric.crypto(ready, owner, costs.aead(bytes));
        let arrive = self.fabric.transfer(sealed, &route, bytes);
        let back = (route.len() - 1) as u8;
        self.metrics.add("peer_bytes", bytes * u64::from(back));
        self.route = route;
        self.engine.at(
            arrive,
            Event::Delivered {
                waiter: w,
                page,
                links: links.max(back),
            },
        );
    }

    /// Per-component memory, in bytes.
    pub fn memory(&self) -> Vec<(&'static str, usize)> {
        let flights: usize = self
            .flights
            .values()
            .map(|f| std::mem::size_of::<Flight>() + 8 + f.waiters.capacity() * 24)
            .sum();
        vec![
            ("engine", self.engine.approximate_bytes()),
            ("fabric", self.fabric.approximate_bytes()),
            ("origin", self.origin.approximate_bytes()),
            ("membership", self.membership.approximate_bytes()),
            ("logic", self.logic.approximate_bytes()),
            (
                "node_state",
                self.down.capacity() + self.node_rng.capacity() * std::mem::size_of::<SplitMix64>(),
            ),
            ("workload", self.reads.approximate_bytes()),
            ("flights", flights),
            ("origin_per_page", self.origin_per_page.capacity() * 16),
        ]
    }

    pub fn report(&self, scenario: &str, seed: u64) -> Value {
        let memory = self.memory();
        let total: usize = memory.iter().map(|(_, b)| b).sum();
        let duplicated = self.origin_per_page.values().filter(|c| **c > 1).count();
        let max_per_page = self.origin_per_page.values().copied().max().unwrap_or(0);
        json!({
            "scenario": scenario,
            "nodes": self.config.nodes,
            "seed": seed,
            "duration_ns": self.engine.now(),
            "events": self.engine.processed(),
            "trace_hash": format!("{:016x}", self.engine.trace.value()),
            "metrics": self.metrics.to_json(),
            "origin_per_page": {
                "pages": self.origin_per_page.len(),
                "pages_fetched_more_than_once": duplicated,
                "max_requests_per_page": max_per_page,
                "page_bytes": self.config.page_bytes,
            },
            "invariants": self.invariants.to_json(),
            "memory": {
                "components": memory.iter().map(|(k, v)| ((*k).to_owned(), json!(v))).collect::<serde_json::Map<_, _>>(),
                "accounted_bytes": total,
                "accounted_bytes_per_node": total / self.config.nodes.max(1) as usize,
                "peak_rss_bytes": peak_rss_bytes(),
            },
        })
    }
}
