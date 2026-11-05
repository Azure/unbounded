//! Model fidelity (plan D14, section 9.2): a byte-free interpreter over the real
//! [`WorkerCore`] machines.
//!
//! Every node runs `workers` real [`WorkerCore`]s and a real [`Placement`] over
//! [`phantom::Phantom`] rankings. The interpreter is dispatch-only: it completes each
//! effect the cores emit on the shared FIFO cost model ([`crate::resources`]), priced from
//! `costs.toml`, as an [`Engine`] event, and answers it with the facts of the world (which
//! node holds which page). It never decides where to probe, when to fetch from origin or
//! whom to shed; the cores do. See `dst/MODEL.md` for what is and is not modeled yet.

pub mod coverage;
pub mod phantom;
mod report;

use std::{
    collections::{BTreeMap, BTreeSet},
    num::NonZeroU32,
    rc::Rc,
    time::{Duration, Instant},
};

use racer_core::{
    prefetch::OwnerBudget,
    read::{
        Error, OperationKind, PageBudget, RefreshPurpose, ResponseKind, ServeFacts,
        acquisition::{self, RankAnswer},
        budget::helper_start,
        effect::EffectId,
        flight::FlightLimits,
        hints::{HINT_CAPACITY, HINT_TTL, SuccessorHints},
        peer_error, recoverable,
        serve::{Body, Decision, ServeDecision, ServeRequest},
        walk::{Features, FillConfig, HedgeEnd, Ranked, Reply},
        window::{self, AcquisitionBudgetSpec, Facts, Next, WindowConfig},
        worker::{
            Effect, Mesh, ReadId, RequestId, Routing, ServeView, Served, Types, WindowId,
            WorkerCore, WorkerMsg,
        },
    },
};
use serde_json::Value;
use topology::{Member, Placement, REPLICAS};

pub use report::{Counters, Report};

use crate::{
    choices::Choices,
    engine::{Engine, MILLIS, Nanos, SECONDS, TraceHash},
    invariants::Class,
    resources::{Fabric, Origin, OriginErrors, OriginOutcome},
    scenario::Scenario,
    world::{Failure, Outcome},
};
use phantom::Phantom;

/// A page of an object.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct Page {
    /// Object id.
    pub object: u64,
    /// Page number within the object.
    pub number: u32,
}

impl Page {
    fn key(self) -> [u8; 12] {
        let mut key = [0; 12];
        key[..8].copy_from_slice(&self.object.to_be_bytes());
        key[8..].copy_from_slice(&self.number.to_be_bytes());
        key
    }
}

/// Payload types of the model: rankings are owner lists, answers name the serving node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ModelTypes;

impl Types for ModelTypes {
    type Rank = Vec<u32>;
    type Membership = u64;
    type Answer = u32;
    type Page = Page;
    type Object = u64;
    type Metadata = u64;
}

/// A model node as a topology member: node `n` has the 4-byte big-endian id of `n`, so member
/// positions are node ids.
#[derive(Clone, Copy, Debug)]
struct ModelMember([u8; 4]);

impl Member for ModelMember {
    const DOMAIN: &'static str = "racer-sim";

    fn id(&self) -> &[u8] {
        &self.0
    }

    fn weight(&self) -> NonZeroU32 {
        NonZeroU32::MIN
    }
}

type Members = topology::Membership<ModelMember>;

/// `nodes` nodes, starting at `first`, each read every page of `object` once, starting at
/// `at` plus a draw below `window`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Storm {
    /// Earliest start.
    pub at: Nanos,
    /// Object read.
    pub object: u64,
    /// First reader node.
    pub first: u32,
    /// Reader count.
    pub nodes: u32,
    /// Start spread.
    pub window: Nanos,
    /// Read the object as one range through a [`ReadWindow`] instead of page by page.
    pub range: Option<RangeRead>,
}

/// Node `node`'s cache is unavailable from `at` until `until`: its stored pages are lost at
/// `at`, and it answers peer probes with `Miss` (`ServeFacts::cache_available` false) until
/// `until`. Its client reads still run.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Outage {
    /// Affected node.
    pub node: u32,
    /// Start of the outage.
    pub at: Nanos,
    /// End of the outage.
    pub until: Nanos,
}

/// The shape of a client range read ([`racer_core::read::window::WindowConfig`]).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RangeRead {
    /// Pages admitted into memory at once (the client's credits).
    pub window_pages: usize,
    /// Pages past the window that may be held on disk.
    pub spill: usize,
    /// The read holds a scheduler lease, so the window may select providers.
    pub leased: bool,
}

/// A report check: `Err` fails the run as a [`Class::Safety`] invariant.
pub type ReportCheck = Rc<dyn Fn(&Value) -> Result<(), String>>;

/// The workload and node shape of a Model scenario.
#[derive(Clone)]
pub struct ModelSpec {
    /// Workers per node.
    pub workers: u32,
    /// Pages per object.
    pub pages_per_object: u32,
    /// Bytes per page.
    pub page_bytes: u64,
    /// Client read storms.
    pub storms: Vec<Storm>,
    /// Client read deadline.
    pub read_timeout: Nanos,
    /// Pages each node keeps; the least recently used page is evicted past it. 0 is unbounded.
    pub stored_pages: usize,
    /// Injected origin error rates.
    pub origin_errors: OriginErrors,
    /// Cache outages.
    pub outages: Vec<Outage>,
    /// Client page reads refresh the object's metadata through the worker's
    /// `MetadataCohort` first.
    pub metadata: bool,
    /// Report checks, by name.
    pub checks: Vec<(String, ReportCheck)>,
}

impl std::fmt::Debug for ModelSpec {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ModelSpec")
            .field("workers", &self.workers)
            .field("pages_per_object", &self.pages_per_object)
            .field("page_bytes", &self.page_bytes)
            .field("storms", &self.storms)
            .field("stored_pages", &self.stored_pages)
            .field("origin_errors", &self.origin_errors)
            .finish()
    }
}

impl Default for ModelSpec {
    /// racer-sim's `Config::default` node shape: 2 workers, 16 pages of 1 MiB, 10 s reads.
    fn default() -> Self {
        Self {
            workers: 2,
            pages_per_object: 16,
            page_bytes: 1 << 20,
            storms: Vec::new(),
            read_timeout: 10_000 * MILLIS,
            stored_pages: 0,
            origin_errors: OriginErrors::default(),
            metadata: false,
            outages: Vec::new(),
            checks: Vec::new(),
        }
    }
}

impl ModelSpec {
    /// Add a report check.
    pub fn check(
        mut self,
        name: impl Into<String>,
        check: impl Fn(&Value) -> Result<(), String> + 'static,
    ) -> Self {
        self.checks.push((name.into(), Rc::new(check)));
        self
    }
}

type Input = acquisition::Input<Vec<u32>, u64, u32>;

/// Engine events.
enum Ev {
    /// A client on `node` reads `page`: attempt `attempt` of a read that began at `start`.
    Read {
        node: u32,
        page: Page,
        start: Nanos,
        attempt: u8,
    },
    /// Answer acquisition `read` of `(node, worker)`.
    Input {
        node: u32,
        worker: u32,
        read: ReadId,
        input: Input,
    },
    /// A probe from `(from, worker, read, id)` arrives at `to`.
    Probe {
        from: u32,
        worker: u32,
        read: ReadId,
        id: EffectId,
        to: u32,
        page: Page,
        copy_only: bool,
        deadline: Nanos,
    },
    /// Body `body` (a key of `Model::bodies`) landed.
    BodyEnd { body: u64 },
    /// A message between workers of `node`.
    Worker {
        node: u32,
        worker: u32,
        msg: WorkerMsg<Page>,
    },
    /// Expire flight waiters.
    Tick,
    /// A client on `node` opens a range read of `object`.
    Open {
        node: u32,
        object: u64,
        range: RangeRead,
    },
    /// Answer window `window` of `(node, worker)`.
    WindowInput {
        node: u32,
        worker: u32,
        window: WindowId,
        input: window::Input,
    },
    /// The client consumed a delivery: plan more window work.
    Pump {
        node: u32,
        worker: u32,
        window: WindowId,
    },
    /// A cache outage of `node` begins.
    Outage { node: u32 },
    /// Answer metadata refresh `id` of `(node, worker)`.
    MetadataInput {
        node: u32,
        worker: u32,
        id: EffectId,
        result: Result<u64, Error>,
    },
}

/// A client range read.
struct RangeState {
    object: u64,
    start: Nanos,
    delivered: u64,
    total: u64,
    leased: bool,
}

/// Who waits for a serving read's completion.
struct Asker {
    from: u32,
    worker: u32,
    read: ReadId,
    id: EffectId,
}

struct Node {
    cores: Vec<WorkerCore<ModelTypes>>,
    stored: Store,
}

/// A node's published pages, bounded by least-recently-used eviction (racer-sim keeps a
/// bounded page set too; the real disk index arrives in a later phase).
#[derive(Default)]
struct Store {
    /// 0 is unbounded.
    capacity: usize,
    clock: u64,
    used: BTreeMap<Page, u64>,
    order: BTreeMap<u64, Page>,
}

impl Store {
    fn new(capacity: usize) -> Self {
        Self {
            capacity,
            ..Self::default()
        }
    }

    fn contains(&self, page: &Page) -> bool {
        self.used.contains_key(page)
    }

    /// Mark `page` used; `false` when it is not stored.
    fn touch(&mut self, page: Page) -> bool {
        let Some(at) = self.used.get_mut(&page) else {
            return false;
        };
        self.order.remove(at);
        self.clock += 1;
        *at = self.clock;
        self.order.insert(self.clock, page);
        true
    }

    /// Store `page`; returns how many pages were evicted to make room.
    fn insert(&mut self, page: Page) -> u64 {
        if self.touch(page) {
            return 0;
        }
        self.clock += 1;
        self.used.insert(page, self.clock);
        self.order.insert(self.clock, page);
        let mut evicted = 0;
        while self.capacity != 0 && self.used.len() > self.capacity {
            let (_, old) = self.order.pop_first().expect("non-empty");
            self.used.remove(&old);
            evicted += 1;
        }
        evicted
    }

    fn len(&self) -> usize {
        self.used.len()
    }
}

/// A running Model world.
struct Model<'s> {
    spec: &'s ModelSpec,
    engine: Engine<Ev>,
    trace: TraceHash,
    choices: Choices,
    base: Instant,
    fabric: Fabric,
    origin: Origin,
    members: Rc<Members>,
    placement: Rc<Placement<Phantom>>,
    nodes: Vec<Node>,
    /// Client reads by `(node, worker, request)`: start time.
    clients: BTreeMap<(u32, u32, RequestId), (Nanos, u8)>,
    /// Client page reads waiting for their metadata refresh, by `(node, worker, request)`.
    refreshing: BTreeMap<(u32, u32, RequestId), (Page, Nanos, u8)>,
    /// Client range reads by `(node, worker, window)`.
    ranges: BTreeMap<(u32, u32, WindowId), RangeState>,
    /// Serving reads by `(node, worker, request)`.
    serving: BTreeMap<(u32, u32, RequestId), Asker>,
    /// Answered effect ids by `(node, worker)`.
    answered: BTreeSet<(u32, u32, EffectId)>,
    /// Bodies being written, by key.
    bodies: BTreeMap<u64, (u32, u32, Body<Page>)>,
    next_body: u64,
    /// Whether a [`Ev::Tick`] is scheduled.
    ticking: bool,
    report: Report,
    expected_reads: u64,
}

const TAG_EVENT: u64 = 1;
const TAG_READ_END: u64 = 2;
const TICK: Nanos = 50 * MILLIS;
/// Client attempts per read, as in racer-sim.
const MAX_ATTEMPTS: u8 = 4;

/// The owners of `page` under `members` (member positions are node ids).
fn owners(placement: &Placement<Phantom>, members: &Members, page: Page) -> Vec<u32> {
    let mut ranked = placement.rank(members, &page.key());
    ranked.truncate(REPLICAS);
    ranked.into_iter().map(|p| p as u32).collect()
}

/// The worker of a node that owns `page`'s flight.
fn owner_worker(page: Page, workers: u32) -> u32 {
    (page.object.wrapping_mul(0x9e37_79b9_7f4a_7c15) ^ u64::from(page.number)) as u32 % workers
}

impl<'s> Model<'s> {
    fn new(scenario: &'s Scenario, spec: &'s ModelSpec, choices: Choices) -> Self {
        let n = scenario.nodes.max(1);
        let base = Instant::now();
        let members = Rc::new(
            Members::new((0..n).map(|node| ModelMember(node.to_be_bytes())).collect())
                .expect("valid membership"),
        );
        let phantom = Phantom::new();
        let placement = Rc::new(Placement::with_rankings(4096, phantom));
        let fabric = Fabric::new(n, scenario.sites, &scenario.costs, &choices);
        let origin = Origin::new(&scenario.costs, choices.clone());
        let workers = spec.workers.max(1);
        let nodes = (0..n)
            .map(|node| Node {
                cores: (0..workers)
                    .map(|worker| core(node, worker, workers, &members, &placement, base))
                    .collect(),
                stored: Store::new(spec.stored_pages),
            })
            .collect();
        Self {
            spec,
            engine: Engine::new(),
            trace: TraceHash::default(),
            choices,
            base,
            fabric,
            origin,
            members,
            placement,
            nodes,
            clients: BTreeMap::new(),
            refreshing: BTreeMap::new(),
            ranges: BTreeMap::new(),
            serving: BTreeMap::new(),
            answered: BTreeSet::new(),
            bodies: BTreeMap::new(),
            next_body: 0,
            ticking: false,
            report: Report::default(),
            expected_reads: 0,
        }
    }

    fn instant(&self, at: Nanos) -> Instant {
        self.base + Duration::from_nanos(at)
    }

    fn nanos(&self, at: Instant) -> Nanos {
        at.saturating_duration_since(self.base).as_nanos() as Nanos
    }

    fn now(&self) -> Nanos {
        self.engine.now()
    }

    /// Count coverage name `name`; a name [`coverage::VARIANTS`] lacks is a hard failure.
    fn cover(&mut self, name: &'static str) {
        if !coverage::VARIANTS.contains(&name) {
            self.violation(format!("coverage name {name} is not in coverage::VARIANTS"));
        }
        *self.report.coverage.entry(name).or_default() += 1;
    }

    fn violation(&mut self, message: String) {
        self.report.violation(message);
    }

    fn schedule_storms(&mut self) {
        for outage in self.spec.outages.clone() {
            self.engine.at(outage.at, Ev::Outage { node: outage.node });
        }
        let n = self.nodes.len() as u32;
        for storm in self.spec.storms.clone() {
            for i in 0..storm.nodes {
                let node = (storm.first + i) % n;
                let at = storm.at + self.choices.below(storm.window.max(1));
                if let Some(range) = storm.range {
                    let object = storm.object;
                    self.engine.at(
                        at,
                        Ev::Open {
                            node,
                            object,
                            range,
                        },
                    );
                    self.expected_reads += u64::from(self.spec.pages_per_object);
                    continue;
                }
                for number in 0..self.spec.pages_per_object {
                    let page = Page {
                        object: storm.object,
                        number,
                    };
                    let read = Ev::Read {
                        node,
                        page,
                        start: at,
                        attempt: 0,
                    };
                    self.engine.at(at, read);
                    self.expected_reads += 1;
                }
            }
        }
    }

    /// Keep flight waiters expiring while reads run.
    fn tick_soon(&mut self) {
        if !self.ticking {
            self.ticking = true;
            self.engine.after(TICK, Ev::Tick);
        }
    }

    fn run(&mut self, end: Nanos, max_steps: u64) {
        while let Some(event) = self.engine.pop_until(end) {
            if self.engine.processed() > max_steps {
                break;
            }
            self.step(event);
        }
    }

    fn step(&mut self, event: Ev) {
        let now = self.now();
        match event {
            Ev::Read {
                node,
                page,
                start,
                attempt,
            } => {
                self.trace.record(now, TAG_EVENT, 1, u64::from(node));
                self.tick_soon();
                if self.spec.metadata {
                    self.refresh(node, page, start, attempt);
                } else {
                    self.client_read(node, page, start, attempt);
                }
            }
            Ev::Open {
                node,
                object,
                range,
            } => {
                self.trace.record(now, TAG_EVENT, 6, u64::from(node));
                self.tick_soon();
                self.open(node, object, range);
            }
            Ev::WindowInput {
                node,
                worker,
                window,
                input,
            } => {
                self.trace.record(now, TAG_EVENT, 7, u64::from(node));
                let mut out = Vec::new();
                let at = self.instant(now);
                // A closed window refuses its late answers.
                let _ = self.nodes[node as usize].cores[worker as usize]
                    .on_window(window, input, true, at, &mut out);
                self.dispatch(node, worker, out);
                self.pump(node, worker, window);
            }
            Ev::Pump {
                node,
                worker,
                window,
            } => self.pump(node, worker, window),
            Ev::Outage { node } => {
                self.trace.record(now, TAG_EVENT, 9, u64::from(node));
                let capacity = self.spec.stored_pages;
                let lost = self.nodes[node as usize].stored.len() as u64;
                self.report.counters.outage_lost_pages += lost;
                self.nodes[node as usize].stored = Store::new(capacity);
            }
            Ev::MetadataInput {
                node,
                worker,
                id,
                result,
            } => {
                self.trace.record(now, TAG_EVENT, 8, u64::from(node));
                let mut out = Vec::new();
                let at = self.instant(now);
                if self.nodes[node as usize].cores[worker as usize]
                    .on_metadata(id, result, at, &mut out)
                    .is_err()
                {
                    self.violation(format!("node {node} refused metadata answer {id:?}"));
                }
                self.dispatch(node, worker, out);
            }
            Ev::Input {
                node,
                worker,
                read,
                input,
            } => {
                self.trace.record(now, TAG_EVENT, 2, u64::from(node));
                let mut out = Vec::new();
                let at = self.instant(now);
                // A stopped acquisition refuses its late answers.
                let _ = self.nodes[node as usize].cores[worker as usize]
                    .on_acquisition(read, input, at, &mut out);
                self.dispatch(node, worker, out);
                self.pump_all(node, worker);
            }
            Ev::Probe {
                from,
                worker,
                read,
                id,
                to,
                page,
                copy_only,
                deadline,
            } => {
                self.trace.record(now, TAG_EVENT, 3, u64::from(to));
                self.serve(
                    Asker {
                        from,
                        worker,
                        read,
                        id,
                    },
                    to,
                    page,
                    copy_only,
                    deadline,
                );
            }
            Ev::BodyEnd { body } => {
                self.trace.record(now, TAG_EVENT, 4, body);
                match self.bodies.remove(&body) {
                    Some((node, worker, body)) => {
                        self.nodes[node as usize].cores[worker as usize]
                            .serve
                            .end_body(body);
                    }
                    None => self.violation(format!("body {body} ended twice")),
                }
            }
            Ev::Worker { node, worker, msg } => {
                self.trace.record(now, TAG_EVENT, 5, u64::from(node));
                let mut out = Vec::new();
                let at = self.instant(now);
                // A fetch answer may arrive after its window closed (the range read failed);
                // the core refuses it. A refused fetch request is a hard failure.
                let late = matches!(msg, WorkerMsg::Fetched { .. });
                if self.nodes[node as usize].cores[worker as usize]
                    .on_worker_msg(msg, at, &mut out)
                    .is_err()
                    && !late
                {
                    self.violation(format!("node {node} worker {worker} refused a message"));
                }
                self.dispatch(node, worker, out);
                self.pump_all(node, worker);
            }
            Ev::Tick => {
                self.ticking = false;
                let at = self.instant(now);
                for node in 0..self.nodes.len() {
                    for worker in 0..self.nodes[node].cores.len() {
                        let mut out = Vec::new();
                        self.nodes[node].cores[worker].tick(at, &mut out);
                        self.dispatch(node as u32, worker as u32, out);
                        self.pump_all(node as u32, worker as u32);
                    }
                }
                if !self.clients.is_empty() || !self.serving.is_empty() {
                    self.tick_soon();
                }
            }
        }
    }

    /// Start client read attempt `attempt` of `page` on `node`.
    fn client_read(&mut self, node: u32, page: Page, start: Nanos, attempt: u8) {
        let now = self.now();
        if self.nodes[node as usize].stored.touch(page) {
            self.report.counters.local_hits += 1;
            self.report.counters.reads_completed += 1;
            self.report.latency(now - start);
            return;
        }
        let worker = owner_worker(page, self.spec.workers.max(1));
        let budget = AcquisitionBudgetSpec {
            deadline: self.instant(now + self.spec.read_timeout),
            attempts: 8,
            links: 16,
        };
        let at = self.instant(now);
        let core = &mut self.nodes[node as usize].cores[worker as usize];
        if core.acquiring(&page) {
            self.report.counters.flight_joins += 1;
        }
        let mut out = Vec::new();
        let request = core.read_page(page, budget, at, &mut out);
        self.clients
            .insert((node, worker, request), (start, attempt));
        self.dispatch(node, worker, out);
    }

    /// Refresh `page`'s object metadata through the owning worker's cohort, then read.
    fn refresh(&mut self, node: u32, page: Page, start: Nanos, attempt: u8) {
        let now = self.now();
        let worker = owner_worker(page, self.spec.workers.max(1));
        let (at, deadline) = (
            self.instant(now),
            self.instant(now + self.spec.read_timeout),
        );
        let mut out = Vec::new();
        let request = self.nodes[node as usize].cores[worker as usize].refresh_metadata(
            page.object,
            RefreshPurpose::Current,
            deadline,
            3,
            at,
            &mut out,
        );
        self.refreshing
            .insert((node, worker, request), (page, start, attempt));
        self.dispatch(node, worker, out);
    }

    /// Open a client range read of `object` on `node`. Its window lives on worker
    /// `node % workers`, so pages owned by the other workers cross as worker fetches.
    fn open(&mut self, node: u32, object: u64, range: RangeRead) {
        let now = self.now();
        let worker = node % self.spec.workers.max(1);
        let config = WindowConfig {
            start: 0,
            end: u64::from(self.spec.pages_per_object),
            window_pages: range.window_pages.max(1),
            sealed: false,
            passthrough: false,
            spill: range.spill,
            budget: PageBudget::Client {
                deadline: self.instant(now + self.spec.read_timeout),
            },
        };
        let key_of = Box::new(move |number: u64| Page {
            object,
            number: number as u32,
        });
        let window = self.nodes[node as usize].cores[worker as usize].open(config, key_of);
        let state = RangeState {
            object,
            start: now,
            delivered: 0,
            total: u64::from(self.spec.pages_per_object),
            leased: range.leased,
        };
        self.ranges.insert((node, worker, window), state);
        self.pump(node, worker, window);
    }

    /// Pump every open window of `(node, worker)`: a fill that ended inside the core (a
    /// flight settled, a worker fetch answered) may have freed or failed a slot.
    fn pump_all(&mut self, node: u32, worker: u32) {
        let windows: Vec<_> = self
            .ranges
            .range((node, worker, WindowId(0))..=(node, worker, WindowId(u64::MAX)))
            .map(|(key, _)| key.2)
            .collect();
        for window in windows {
            self.pump(node, worker, window);
        }
    }

    /// Plan window work and deliver what is ready.
    fn pump(&mut self, node: u32, worker: u32, window: WindowId) {
        let Some(state) = self.ranges.get(&(node, worker, window)) else {
            return;
        };
        let facts = Facts {
            leased: state.leased,
            ..Facts::default()
        };
        let at = self.instant(self.now());
        let mut out = Vec::new();
        let _ = self.nodes[node as usize].cores[worker as usize].pump(
            window,
            at,
            &facts,
            |_| true,
            &mut out,
        );
        self.dispatch(node, worker, out);
    }

    /// Complete window effect `effect` of `(node, worker, window)`.
    fn window_effect(&mut self, node: u32, worker: u32, window: WindowId, effect: window::Effect) {
        use window::Effect as Win;
        let now = self.now();
        let costs = self.fabric.costs;
        let Some(object) = self.ranges.get(&(node, worker, window)).map(|r| r.object) else {
            // Effects of a closed window (releases, discards) need no answer.
            return;
        };
        let key = |number: u64| Page {
            object,
            number: number as u32,
        };
        let answer = |input| Ev::WindowInput {
            node,
            worker,
            window,
            input,
        };
        match effect {
            Win::Probe { id, page, .. } => {
                let found = self.nodes[node as usize].stored.touch(key(page));
                let at = if found {
                    self.report.counters.disk_reads += 1;
                    self.fabric.disk_read(now, node, self.spec.page_bytes)
                } else {
                    now + costs.cpu_request_ns
                };
                self.engine
                    .at(at, answer(window::Input::Probed { id, found }));
            }
            // Provider selection is not modeled: the selection cannot start and its pages
            // are filled directly.
            Win::Select { id, .. } => {
                let input = window::Input::Selected {
                    id,
                    ok: false,
                    started: false,
                };
                self.engine.at(now + costs.cpu_request_ns, answer(input));
            }
            Win::Hold { id, .. } => {
                self.report.counters.spill_holds += 1;
                let at = self.fabric.disk_write(now, node, 0);
                let input = window::Input::Held {
                    id,
                    result: Ok(id.0),
                };
                self.engine.at(at, answer(input));
            }
            // The held page is persisted when the node already stores it; otherwise the spill
            // fill fails and the window fills the page directly.
            Win::SpillFill { id, page } => {
                let ok = self.nodes[node as usize].stored.contains(&key(page));
                let at = self.fabric.disk_write(now, node, self.spec.page_bytes);
                self.engine
                    .at(at, answer(window::Input::SpillFilled { id, ok }));
            }
            Win::Fetch { .. } => self.violation(format!("node {node} leaked a window fetch")),
            Win::Release(_) | Win::Discard(_) => {}
        }
    }

    /// The client of `(node, worker, window)` received `next`.
    fn deliver(&mut self, node: u32, worker: u32, window: WindowId, next: Next) {
        let now = self.now();
        let Some(state) = self.ranges.get_mut(&(node, worker, window)) else {
            return self.violation(format!("node {node} delivered to a closed window"));
        };
        match next {
            Next::Wait => {}
            Next::Page { .. } => {
                state.delivered += 1;
                let start = state.start;
                self.report.counters.reads_completed += 1;
                self.report.latency(now - start);
                let at = now + self.fabric.costs.cpu_request_ns;
                let pump = Ev::Pump {
                    node,
                    worker,
                    window,
                };
                self.engine.at(at, pump);
            }
            Next::End | Next::Fail(_) => {
                let state = self
                    .ranges
                    .remove(&(node, worker, window))
                    .expect("present");
                let missing = state.total - state.delivered;
                if let Next::Fail(error) = next {
                    self.report.counters.reads_failed += missing;
                    *self.report.errors.entry(format!("{error:?}")).or_default() += missing;
                } else if missing != 0 {
                    self.violation(format!("window ended with {missing} pages undelivered"));
                }
                let at = self.instant(now);
                let mut out = Vec::new();
                let core = &mut self.nodes[node as usize].cores[worker as usize];
                core.close(window, at, &mut out);
                core.drop_window(window);
                self.dispatch(node, worker, out);
            }
        }
    }

    /// Answer `asker`'s probe at node `to`.
    fn serve(&mut self, asker: Asker, to: u32, page: Page, copy_only: bool, deadline: Nanos) {
        let now = self.now();
        let worker = owner_worker(page, self.spec.workers.max(1));
        let owners = owners(&self.placement, &self.members, page);
        let stored = self.nodes[to as usize].stored.contains(&page);
        let at = self.instant(now);
        let core = &mut self.nodes[to as usize].cores[worker as usize];
        let request = ServeRequest {
            facts: ServeFacts {
                operation: OperationKind::Page,
                copy_only,
                object_matches: true,
                cache_available: !self
                    .spec
                    .outages
                    .iter()
                    .any(|o| o.node == to && (o.at..o.until).contains(&now)),
            },
            body: true,
            local_flight: core.acquiring(&page),
        };
        let view = ServeView {
            owners: &owners,
            local: to,
            requester: asker.from,
            column_mate: false,
        };
        let served = core.serve(&page, request, &view, at);
        self.cover(served_name(&served));
        let decision = match served {
            Served::Defer(_) => {
                self.report.counters.deferrals += 1;
                return self.reply(&asker, to, ResponseKind::Miss);
            }
            Served::Decided(decision) => decision,
        };
        match decision {
            Decision::Shed => {
                self.report.counters.sheds += 1;
                self.reply(&asker, to, ResponseKind::Overloaded)
            }
            Decision::Reject(_) | Decision::Miss => self.reply(&asker, to, ResponseKind::Miss),
            _ if stored => {
                self.nodes[to as usize].stored.touch(page);
                self.send_page(asker, to, page)
            }
            Decision::CopyOnly { .. } => self.reply(&asker, to, ResponseKind::Miss),
            Decision::Acquire => {
                let budget = AcquisitionBudgetSpec {
                    deadline: self.instant(deadline.max(now + 1)),
                    attempts: 4,
                    links: 8,
                };
                let mut out = Vec::new();
                let core = &mut self.nodes[to as usize].cores[worker as usize];
                if core.acquiring(&page) {
                    self.report.counters.flight_joins += 1;
                }
                let request = core.read_page(page, budget, at, &mut out);
                self.serving.insert((to, worker, request), asker);
                self.dispatch(to, worker, out);
            }
        }
    }

    /// `to` streams its copy of `page` to the asker; the reply lands when the body does.
    fn send_page(&mut self, asker: Asker, to: u32, page: Page) {
        let now = self.now();
        let worker = owner_worker(page, self.spec.workers.max(1));
        let body = self.nodes[to as usize].cores[worker as usize]
            .serve
            .begin_body(page);
        self.next_body += 1;
        let key = self.next_body;
        self.bodies.insert(key, (to, worker, body));
        let end = self
            .fabric
            .transfer(now, &[to, asker.from], self.spec.page_bytes);
        self.report.counters.peer_bytes += self.spec.page_bytes;
        self.report.counters.peer_pages += 1;
        if !owners(&self.placement, &self.members, page).contains(&to) {
            self.report.counters.helper_copies += 1;
        }
        self.engine.at(end, Ev::BodyEnd { body: key });
        self.engine.at(
            end,
            Ev::Input {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                input: Input::Replied {
                    id: asker.id,
                    reply: Reply::Answer {
                        kind: ResponseKind::Page,
                        signed: true,
                    },
                    answer: Some(to),
                },
            },
        );
    }

    /// The serving read on `to` failed without a peer response: a transport failure.
    fn fail_probe(&mut self, asker: &Asker, to: u32) {
        let at = self.fabric.message(self.now(), to, asker.from);
        self.engine.at(
            at,
            Ev::Input {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                input: Input::Replied {
                    id: asker.id,
                    reply: Reply::Failed {
                        error: Error::Unavailable,
                        sent: true,
                        scope_live: true,
                    },
                    answer: None,
                },
            },
        );
    }

    fn reply(&mut self, asker: &Asker, to: u32, kind: ResponseKind) {
        let at = self.fabric.message(self.now(), to, asker.from);
        self.engine.at(
            at,
            Ev::Input {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                input: Input::Replied {
                    id: asker.id,
                    reply: Reply::Answer { kind, signed: true },
                    answer: None,
                },
            },
        );
    }

    /// Complete each effect on the cost model. Dispatch only: the answer is a fact of the
    /// world, never a choice of where to go next.
    fn dispatch(&mut self, node: u32, worker: u32, effects: Vec<Effect<ModelTypes>>) {
        for effect in effects {
            self.cover(effect_name(&effect));
            if let Some(id) = effect.id()
                && !self.answered.insert((node, worker, id))
            {
                self.violation(format!("node {node} worker {worker} reused effect {id:?}"));
            }
            self.complete(node, worker, effect);
        }
    }

    fn complete(&mut self, node: u32, worker: u32, effect: Effect<ModelTypes>) {
        use acquisition::Io as Acq;
        let now = self.now();
        let costs = self.fabric.costs;
        let input = |read, input| Ev::Input {
            node,
            worker,
            read,
            input,
        };
        match effect {
            Effect::Acquisition { read, page, effect } => match effect {
                Acq::Rank { id, .. } => {
                    let owners = owners(&self.placement, &self.members, page);
                    let rank = owners.iter().position(|o| *o == node);
                    let at = self
                        .fabric
                        .cpu(now, node, worker as u8, costs.cpu_ranking_chunk_ns);
                    let ranked = Ranked {
                        members: self.nodes.len(),
                        ..Ranked::new(1, &owners, rank)
                    };
                    let answer = Input::Ranked {
                        id,
                        result: Ok(RankAnswer {
                            ranked,
                            ranking: Some(owners),
                            recent: None,
                        }),
                    };
                    self.engine.at(at, input(read, answer));
                }
                Acq::Hints { id, .. } => {
                    let answer = Input::Hints {
                        id,
                        hints: Vec::new(),
                        rank0: Default::default(),
                    };
                    self.engine
                        .at(now + costs.cpu_request_ns, input(read, answer));
                }
                Acq::Hedge { id, .. } => {
                    let answer = Input::Hedged {
                        id,
                        end: HedgeEnd::NotStarted,
                        copy: false,
                    };
                    self.engine.at(now, input(read, answer));
                }
                Acq::Refresh { id, .. } => {
                    let answer = Input::Membership {
                        id,
                        latest: Ok(None),
                        recent_previous: None,
                    };
                    self.engine
                        .at(now + costs.cpu_request_ns, input(read, answer));
                }
                Acq::Send {
                    id, probe, dest, ..
                } => {
                    self.report.counters.peer_probes += 1;
                    let Some(to) = dest.filter(|d| (*d as usize) < self.nodes.len()) else {
                        let failed = Input::Replied {
                            id,
                            reply: Reply::Failed {
                                error: Error::Unavailable,
                                sent: false,
                                scope_live: true,
                            },
                            answer: None,
                        };
                        self.engine.at(now, input(read, failed));
                        return;
                    };
                    // The serving read inherits half the client deadline (see MODEL.md).
                    let deadline = now + self.spec.read_timeout / 2;
                    let at = self.fabric.message(now, node, to) + costs.cpu_request_ns;
                    self.engine.at(
                        at,
                        Ev::Probe {
                            from: node,
                            worker,
                            read,
                            id,
                            to,
                            page,
                            copy_only: probe.mode == racer_core::read::walk::Mode::CopyOnly,
                            deadline,
                        },
                    );
                }
                Acq::Validate { id, .. } => {
                    let at = self
                        .fabric
                        .crypto(now, node, costs.open(self.spec.page_bytes));
                    let answer = Input::Validated {
                        id,
                        result: Ok(None),
                    };
                    self.engine.at(at, input(read, answer));
                }
                Acq::Batch { id, .. } => {
                    let answer = Input::Batched {
                        id,
                        result: Ok(false),
                    };
                    self.engine
                        .at(now + costs.cpu_request_ns, input(read, answer));
                }
                Acq::Sleep { id, until } => {
                    let at = self.nanos(until).max(now);
                    self.engine
                        .at(at, input(read, Input::Timer { id, result: Ok(()) }));
                }
                Acq::Origin { id, .. } => {
                    self.report.counters.origin_requests += 1;
                    *self.report.origin_per_page.entry(page).or_default() += 1;
                    let (at, outcome) = self.origin.request(
                        now,
                        page.object,
                        self.spec.page_bytes,
                        self.spec.origin_errors,
                    );
                    // racer-sim's mapping (`sim/src/node/world.rs`, origin completion).
                    let c = &mut self.report.counters;
                    let result = match outcome {
                        OriginOutcome::Ok => {
                            c.origin_bytes += self.spec.page_bytes;
                            Ok(())
                        }
                        OriginOutcome::PreconditionFailed => {
                            c.origin_412 += 1;
                            Err(Error::VersionUnavailable)
                        }
                        OriginOutcome::Forbidden => {
                            c.origin_403 += 1;
                            Err(Error::OriginForbidden)
                        }
                        OriginOutcome::ServerError => {
                            c.origin_5xx += 1;
                            Err(Error::Unavailable)
                        }
                    };
                    self.engine
                        .at(at, input(read, Input::Origin { id, result }));
                }
                Acq::Publish { id, .. } => {
                    self.report.counters.disk_writes += 1;
                    let at = self.fabric.disk_write(now, node, self.spec.page_bytes);
                    let evicted = self.nodes[node as usize].stored.insert(page);
                    self.report.counters.evictions += evicted;
                    self.engine
                        .at(at, input(read, Input::Published { id, result: Ok(()) }));
                }
            },
            Effect::Completed {
                request,
                page,
                result,
            } => self.completed(node, worker, request, page, result),
            Effect::Worker { to, msg } => {
                let at = now + costs.cpu_request_ns;
                self.engine.at(
                    at,
                    Ev::Worker {
                        node,
                        worker: to,
                        msg,
                    },
                );
            }
            Effect::Window { window, effect } => self.window_effect(node, worker, window, effect),
            Effect::Deliver { window, next } => self.deliver(node, worker, window, next),
            Effect::Metadata { id, key } => {
                // A metadata HEAD: origin latency and errors, no body.
                self.report.counters.metadata_requests += 1;
                let (at, outcome) =
                    self.origin
                        .request(now, key.object, 0, self.spec.origin_errors);
                let result = match outcome {
                    OriginOutcome::Ok => Ok(1),
                    OriginOutcome::PreconditionFailed => Err(Error::VersionUnavailable),
                    OriginOutcome::Forbidden => Err(Error::OriginForbidden),
                    OriginOutcome::ServerError => Err(Error::Unavailable),
                };
                let answer = Ev::MetadataInput {
                    node,
                    worker,
                    id,
                    result,
                };
                self.engine.at(at, answer);
            }
            Effect::MetadataReady { request, result } => {
                let Some((page, start, attempt)) = self.refreshing.remove(&(node, worker, request))
                else {
                    return self.violation(format!("node {node} unknown metadata {request:?}"));
                };
                match result {
                    Ok(_) => self.client_read(node, page, start, attempt),
                    Err(error) => self.client_failed(node, page, start, attempt, error),
                }
            }
        }
    }

    fn completed(
        &mut self,
        node: u32,
        worker: u32,
        request: RequestId,
        page: Page,
        result: Result<(), Error>,
    ) {
        let now = self.now();
        if let Some(asker) = self.serving.remove(&(node, worker, request)) {
            if result.is_ok() && self.nodes[node as usize].stored.contains(&page) {
                self.send_page(asker, node, page);
            } else {
                // Production `peer_error` (`src/read.rs`, racer-sim `err_reply`): an expected
                // acquisition failure is answered with its response kind; any other is a
                // transport failure.
                let error = result.err().unwrap_or(Error::Unavailable);
                *self
                    .report
                    .attempt_errors
                    .entry(format!("serve:{error:?}"))
                    .or_default() += 1;
                match peer_error(error) {
                    Some(kind) => self.reply(&asker, node, kind),
                    None => self.fail_probe(&asker, node),
                }
            }
            return;
        }
        let Some((start, attempt)) = self.clients.remove(&(node, worker, request)) else {
            self.violation(format!("node {node} completed unknown read {request:?}"));
            return;
        };
        self.trace.record(
            now,
            TAG_READ_END,
            u64::from(node),
            u64::from(result.is_ok()),
        );
        match result {
            Ok(()) => {
                self.report.counters.reads_completed += 1;
                self.report.latency(now - start);
            }
            Err(error) => self.client_failed(node, page, start, attempt, error),
        }
    }

    /// A client read attempt failed with `error`: retry or fail the read.
    fn client_failed(&mut self, node: u32, page: Page, start: Nanos, attempt: u8, error: Error) {
        let elapsed = (self.now() - start) / SECONDS;
        *self
            .report
            .attempt_errors
            .entry(format!("{error:?}#{attempt}@{elapsed}s"))
            .or_default() += 1;
        match error {
            // racer-sim's client retries a recoverable failure (`resolve` in
            // `sim/src/node/world.rs`) after a linear backoff.
            error if recoverable(error) && attempt + 1 < MAX_ATTEMPTS => {
                self.report.counters.read_retries += 1;
                let backoff = 2 * self.fabric.rtt.rtt(0, 0).max(MILLIS) * u64::from(attempt + 1);
                let retry = Ev::Read {
                    node,
                    page,
                    start,
                    attempt: attempt + 1,
                };
                self.engine.after(backoff, retry);
            }
            error => {
                self.report.counters.reads_failed += 1;
                *self.report.errors.entry(format!("{error:?}")).or_default() += 1;
            }
        }
    }

    /// End-of-run checks: every read ended and every core is quiescent.
    fn finish(&mut self) {
        let ended = self.report.counters.reads_completed + self.report.counters.reads_failed;
        if ended != self.expected_reads {
            self.violation(format!("{} of {} reads ended", ended, self.expected_reads));
        }
        for (n, node) in self.nodes.iter().enumerate() {
            let stored = node.stored.len() as u64;
            let max = &mut self.report.counters.stored_pages_max;
            *max = (*max).max(stored);
            for (w, core) in node.cores.iter().enumerate() {
                let refreshes = core.metadata().0;
                if core.flights() + core.requests() + core.reads() + core.windows() + refreshes != 0
                {
                    let message = format!(
                        "node {n} worker {w} not quiescent: {} flights {} requests {} reads",
                        core.flights(),
                        core.requests(),
                        core.reads()
                    );
                    self.report.violation(message);
                }
                let observed = core.observed();
                self.report.counters.helpers += observed.helpers;
                self.report.counters.no_helper += observed.no_helper;
            }
        }
        if !self.ranges.is_empty() || !self.refreshing.is_empty() {
            let (r, m) = (self.ranges.len(), self.refreshing.len());
            self.violation(format!(
                "{r} range reads and {m} metadata refreshes unended"
            ));
        }
        if !self.serving.is_empty() {
            self.violation(format!("{} serving reads unanswered", self.serving.len()));
        }
        if !self.bodies.is_empty() {
            self.violation(format!("{} bodies never ended", self.bodies.len()));
        }
    }
}

/// The coverage name of an effect: the variant path, exhaustively, so a new core variant fails
/// to compile here until it has a name (see [`coverage`]).
pub fn effect_name(effect: &Effect<ModelTypes>) -> &'static str {
    use acquisition::Io as Acq;
    use racer_core::read::window::{Effect as Win, Next};
    match effect {
        Effect::Acquisition { effect, .. } => match effect {
            Acq::Rank { .. } => "Acquisition::Rank",
            Acq::Hints { .. } => "Acquisition::Hints",
            Acq::Hedge { .. } => "Acquisition::Hedge",
            Acq::Refresh { .. } => "Acquisition::Refresh",
            Acq::Send { .. } => "Acquisition::Send",
            Acq::Validate { .. } => "Acquisition::Validate",
            Acq::Batch { .. } => "Acquisition::Batch",
            Acq::Sleep { .. } => "Acquisition::Sleep",
            Acq::Origin { .. } => "Acquisition::Origin",
            Acq::Publish { .. } => "Acquisition::Publish",
        },
        Effect::Window { effect, .. } => match effect {
            Win::Probe { .. } => "Window::Probe",
            Win::Fetch { .. } => "Window::Fetch",
            Win::Select { .. } => "Window::Select",
            Win::Hold { .. } => "Window::Hold",
            Win::SpillFill { .. } => "Window::SpillFill",
            Win::Release(_) => "Window::Release",
            Win::Discard(_) => "Window::Discard",
        },
        Effect::Deliver { next, .. } => match next {
            Next::End => "Deliver::End",
            Next::Wait => "Deliver::Wait",
            Next::Page { .. } => "Deliver::Page",
            Next::Fail(_) => "Deliver::Fail",
        },
        Effect::Worker { msg, .. } => match msg {
            WorkerMsg::Fetch { .. } => "Worker::Fetch",
            WorkerMsg::Fetched { .. } => "Worker::Fetched",
        },
        Effect::Completed { result: Ok(()), .. } => "Completed::Ok",
        Effect::Completed { result: Err(_), .. } => "Completed::Err",
        Effect::Metadata { .. } => "Metadata",
        Effect::MetadataReady { result: Ok(_), .. } => "MetadataReady::Ok",
        Effect::MetadataReady { result: Err(_), .. } => "MetadataReady::Err",
    }
}

/// The coverage name of a serve-side outcome.
fn served_name(served: &Served) -> &'static str {
    match served {
        Served::Defer(_) => "Served::Defer",
        Served::Decided(Decision::Reject(_)) => "Served::Reject",
        Served::Decided(Decision::Miss) => "Served::Miss",
        Served::Decided(Decision::Shed) => "Served::Shed",
        Served::Decided(Decision::CopyOnly { .. }) => "Served::CopyOnly",
        Served::Decided(Decision::Acquire) => "Served::Acquire",
    }
}

/// One real [`WorkerCore`] for `(node, worker)`.
fn core(
    node: u32,
    worker: u32,
    workers: u32,
    members: &Rc<Members>,
    placement: &Rc<Placement<Phantom>>,
    base: Instant,
) -> WorkerCore<ModelTypes> {
    let (m, p) = (members.clone(), placement.clone());
    let plan = Box::new(move |page: &Page, budget: &AcquisitionBudgetSpec| {
        let config = FillConfig {
            operation: OperationKind::Page,
            hedge: false,
            walk_only: false,
            scope: budget.deadline,
            features: Features::PRODUCTION,
        };
        (config, owners(&p, &m, *page))
    });
    let (m, p) = (members.clone(), placement.clone());
    let mesh = Box::new(move |page: &Page, _version: u64| {
        Some(Mesh {
            owners: owners(&p, &m, *page),
            start: helper_start(&node.to_be_bytes()),
            reader: node,
        })
    });
    let m = members.clone();
    let helper_of = Box::new(move |owner: u32| {
        let pool = m.pools().first()?;
        pool.helper(node as usize, owner as usize).map(|h| h as u32)
    });
    WorkerCore::new(
        Routing {
            me: worker,
            owner_of: Box::new(move |page| owner_worker(*page, workers)),
            plan,
            mesh,
            helper_of,
        },
        FlightLimits::default(),
        64,
        ServeDecision::new(1 << 20),
        SuccessorHints::new(HINT_CAPACITY, HINT_TTL),
        OwnerBudget::new(4, 0),
        base,
    )
}

/// Run a Model scenario over `choices`: the outcome and the JSON report.
pub fn run(scenario: &Scenario, choices: Choices) -> (Outcome, Value) {
    let Some(spec) = scenario.model.as_deref() else {
        let failure = Failure {
            class: Class::Safety,
            name: "fidelity".into(),
            at: 0,
            step: 0,
            message: "Model fidelity needs a ModelSpec".into(),
        };
        return (
            Outcome {
                trace: 0,
                steps: 0,
                end: 0,
                draws: choices.drawn(),
                quiescent: false,
                failure: Some(failure),
            },
            Value::Null,
        );
    };
    let mut model = Model::new(scenario, spec, choices.clone());
    model.schedule_storms();
    model.run(scenario.end, scenario.max_steps);
    let quiescent = model.engine.pending() == 0;
    model.finish();
    let report = model
        .report
        .json(scenario, spec, choices.seed(), model.now());
    let steps = model.engine.processed();
    let mut failure = None;
    let hard = report["invariants"]["hard_failures"].as_u64().unwrap_or(0);
    if hard != 0 {
        failure = Some(Failure {
            class: Class::Safety,
            name: "hard-failures".into(),
            at: model.now(),
            step: steps,
            message: report["invariants"].to_string(),
        });
    }
    for (name, check) in &spec.checks {
        if failure.is_some() {
            break;
        }
        if let Err(message) = check(&report) {
            failure = Some(Failure {
                class: Class::Safety,
                name: name.clone(),
                at: model.now(),
                step: steps,
                message,
            });
        }
    }
    let mut trace = model.trace;
    trace.record(
        model.now(),
        3,
        steps,
        report["metrics"]["counters"].to_string().len() as u64,
    );
    trace.record_bytes(model.now(), 4, report["metrics"].to_string().as_bytes());
    (
        Outcome {
            trace: trace.value(),
            steps,
            end: model.now(),
            draws: choices.drawn(),
            quiescent,
            failure,
        },
        report,
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn page(number: u32) -> Page {
        Page { object: 0, number }
    }

    #[test]
    fn store_evicts_the_least_recently_used_page() {
        let mut store = Store::new(2);
        assert_eq!(store.insert(page(1)), 0);
        assert_eq!(store.insert(page(2)), 0);
        assert!(store.touch(page(1)));
        assert_eq!(store.insert(page(3)), 1);
        assert!(store.contains(&page(1)) && store.contains(&page(3)));
        assert!(!store.contains(&page(2)) && !store.touch(page(2)));
        assert_eq!(store.insert(page(3)), 0, "a re-publish refreshes");
        assert_eq!(store.len(), 2);
    }

    #[test]
    fn an_unbounded_store_never_evicts() {
        let mut store = Store::new(0);
        for n in 0..100 {
            assert_eq!(store.insert(page(n)), 0);
        }
        assert_eq!(store.len(), 100);
    }
}
