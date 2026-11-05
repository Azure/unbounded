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
pub mod membership;
pub mod phantom;
mod report;

use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet},
    rc::Rc,
    time::{Duration, Instant},
};

use racer_control_wire::{CacheId, NodeId};
use racer_core::{
    liveness::{Detours, LINK_TIMEOUTS},
    liveness::{MAX_PINGS_IN_FLIGHT, SILENCE_CAPACITY, Silence},
    prefetch::OwnerBudget,
    read::{
        Answering, Error, OperationKind, PageBudget, RefreshPurpose, ReplicaSelection,
        ResponseKind, ServeFacts, VisitOrder,
        acquisition::{self, RankAnswer, RankBasis},
        budget::{helper_start, inherited_budget, stale_unused_links},
        demote_unanswered,
        effect::EffectId,
        flight::FlightLimits,
        hints::{HINT_CAPACITY, HINT_TTL, NOTICE_LINKS, SuccessorHints},
        peer_error, recoverable, replica_score,
        serve::{Body, Decision, ServeDecision, ServeRequest},
        visit_order,
        walk::{
            AttemptInputs, Features, FillConfig, HedgeEnd, NOT_MEMBER, Ranked, Reply, SilenceNote,
            plan_attempt,
        },
        window::{self, AcquisitionBudgetSpec, Facts, Next, WindowConfig},
        worker::{
            Effect, Mesh, ReadId, RequestId, Routing, ServeView, Served, Types, WindowId,
            WorkerCore, WorkerMsg,
        },
    },
    route::{
        Fault, Hop, MAX_LINKS, RelayFailure, RelayPlan, Response, RouteFault, decide_hop,
        relay_failure, search_budget,
    },
};
use racer_object_wire::model::{CacheKey, ObjectId, PageNumber};
use serde_json::Value;
use topology::{Exhaustive, PathQuery, Paths, Placement, REPLICAS};

pub use report::{Counters, Report};

use crate::{
    choices::Choices,
    engine::{Engine, MILLIS, Nanos, SECONDS, TraceHash},
    invariants::Class,
    resources::{Fabric, Origin, OriginErrors, OriginOutcome},
    scenario::Scenario,
    world::{Failure, Outcome},
};
use membership::{Members, Version, Versions};
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

/// A ranking: its membership version and its owners (boxed to keep [`Ev`] small).
pub type Ranking = Box<(u64, Vec<u32>)>;

/// Payload types of the model: rankings are owner lists, answers name the serving node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ModelTypes;

impl Types for ModelTypes {
    type Rank = Ranking;
    type Membership = u64;
    type Answer = u32;
    type Page = Page;
    type Object = u64;
    type Metadata = u64;
}

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

/// Membership churn (racer-sim `Churn`): `node` joins or leaves at `at`. A node that left keeps
/// running; it is only absent from the memberships published after.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Churn {
    /// When the change is made.
    pub at: Nanos,
    /// The node that joins or leaves.
    pub node: u32,
    /// Joins (`true`) or leaves.
    pub join: bool,
}

/// Random churn (racer-sim `Churn::random`): `events` leave and rejoin pairs, each leave drawn
/// below `within` on a drawn node that rejoins `away` later.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RandomChurn {
    /// Leave and rejoin pairs.
    pub events: u32,
    /// Leaves are drawn below this time.
    pub within: Nanos,
    /// How long each node stays away.
    pub away: Nanos,
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
    /// Per-attempt exchange cap of a peer probe (`AttemptInputs::attempt_timeout`); a probe
    /// unanswered by then times out (`Reply::TimedOut`). Production and racer-sim use
    /// [`ATTEMPT_TIMEOUT`], the default; [`NO_ATTEMPT_CAP`] bounds an attempt by its signed
    /// overall deadline only.
    pub attempt_timeout: Nanos,
    /// Pages each node keeps; the least recently used page is evicted past it. 0 is unbounded.
    pub stored_pages: usize,
    /// Injected origin error rates.
    pub origin_errors: OriginErrors,
    /// Cache outages.
    pub outages: Vec<Outage>,
    /// Hung peer servers (racer-sim `Fault::Hang`): during `[at, until)` the node accepts every
    /// peer request, as destination or relay, and never answers or forwards it.
    pub hangs: Vec<Outage>,
    /// Client page reads refresh the object's metadata through the worker's
    /// `MetadataCohort` first.
    pub metadata: bool,
    /// Membership changes, published in batches of [`membership::BATCH`].
    pub churn: Vec<Churn>,
    /// Drawn membership changes, added to [`Self::churn`].
    pub random_churn: Option<RandomChurn>,
    /// Most adoption lag of a node (racer-sim `membership_lag_ns`, at most one batch).
    pub membership_lag: Nanos,
    /// `Relay` transfers per worker; production's default unless a scenario overrides it
    /// (racer-sim `NodeConfig::relay_transfers`).
    pub relay_transfers: usize,
    /// Waiters per flight; production's default unless a scenario overrides it (racer-sim
    /// `NodeConfig::waiters_per_flight`).
    pub waiters_per_flight: usize,
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
            attempt_timeout: ATTEMPT_TIMEOUT,
            stored_pages: 0,
            origin_errors: OriginErrors::default(),
            metadata: false,
            outages: Vec::new(),
            hangs: Vec::new(),
            churn: Vec::new(),
            random_churn: None,
            membership_lag: membership::LAG,
            relay_transfers: relay_transfers(),
            waiters_per_flight: flight_limits().waiters_per_flight,
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

type Input = acquisition::Input<Ranking, u64, u32>;

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
    /// Peer request `msg` (a key of `Model::msgs`) reaches its next holder (`verified` once the
    /// holder checked its signature and, at the destination, its worker took it).
    Hop { msg: u64, verified: bool },
    /// The requester of `msg` gave up: a last link still unanswered is a timeout for its
    /// sender (racer-sim `Event::LastHop`).
    LastHop { msg: u64 },
    /// Silence ping `ping` (a key of `Model::pings`) ended: [`liveness::ping_verdict`] of its
    /// answer. A ping ends once; later ends are dropped.
    PingEnd { ping: u64, answered: Option<bool> },
    /// Silence ping `ping` reached its attempt timeout.
    PingTimeout { ping: u64 },
    /// `from` sends its due silence pings (production polls them every worker turn).
    PollPings { from: u32 },
    /// Probe `id` of `(node, worker, read)` reached its attempt bound
    /// (`AttemptPlan::attempt_end`) unanswered.
    ProbeTimeout {
        node: u32,
        worker: u32,
        read: ReadId,
        id: EffectId,
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
    /// A `StaleMembership` answer returns `remote` credits and `links` to `(node, worker,
    /// read)`'s request budget, just before the answer.
    Refund {
        node: u32,
        worker: u32,
        read: ReadId,
        remote: u32,
        links: u8,
    },
    /// The relays of `msg` release their `Relay` transfers.
    Release { msg: u64 },
    /// `node` joins (`live`) or leaves the membership.
    Membership { node: u32, live: bool },
    /// Publish the pending membership changes.
    Publish,
    /// A scheduled node or network fault ([`crate::world::Fault`]).
    Fault(crate::world::Fault),
    /// The link from `holder` for `msg` failed to connect.
    LinkDown { msg: u64, holder: u32 },
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

/// A sent probe: destination, page, copy-only, and send time.
type Sent = (u32, Page, bool, Nanos);

/// What a probe carries to its destination.
#[derive(Clone, Copy)]
struct Route {
    /// The membership version the request is signed under (`Io::Send::version`).
    version: u64,
    copy_only: bool,
    deadline: Nanos,
    attempts: u32,
    links: u8,
    /// Links the requester charged (`links` shrinks at each relay).
    charged: u8,
}

/// A peer request on its route (racer-sim `Msg`; production `peer/forwarding.rs`).
struct Msg {
    from: u32,
    worker: u32,
    read: ReadId,
    id: EffectId,
    dest: u32,
    page: Page,
    route: Route,
    /// Wire form: the requester and every relay so far.
    path: Vec<u32>,
    /// The node the request travels to.
    at: u32,
    abandon: Nanos,
    /// A silence ping (a key of `Model::pings`), answered `Miss` at once by its destination.
    ping: Option<u64>,
}

/// Who waits for a serving read's completion.
struct Asker {
    from: u32,
    worker: u32,
    read: ReadId,
    id: EffectId,
    /// When the probe arrived.
    at: Nanos,
    /// When the requester abandons the exchange (its `AttemptPlan::attempt_end`).
    abandon: Nanos,
    /// The request's wire route (requester first); replies travel it in reverse.
    path: Vec<u32>,
    /// The request's destination: an answer from any other node is a relay's.
    dest: u32,
    /// The request's key in `Model::msgs` (and `Model::last_hops`).
    msg: u64,
    /// The membership version the request is signed under.
    version: u64,
    /// Remote credits the request carried (`AttemptPlan::remote_attempts`).
    attempts: u32,
    /// Links the requester charged for the whole route (`AttemptPlan::links`).
    links: u8,
    /// The silence ping this request is.
    ping: Option<u64>,
}

struct Node {
    cores: Vec<WorkerCore<ModelTypes>>,
    stored: Store,
    /// Production `LinkHealth` silence, shared by the node's workers (racer-sim
    /// `NodeState::silence`): fed by [`Effect::Silence`], read by every non-owner ranking.
    silence: Silence<u32, Page>,
    /// Silence pings in flight (production `CandidatePolicy::pings`).
    pings: usize,
    /// Production `LinkHealth` detours (racer-sim `NodeState::detours`): first hops a route
    /// skips after timeouts.
    detours: Detours<u32>,
    /// Crashed and not yet restarted: its peer server takes no connections.
    down: bool,
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
    /// Placement rankings each worker has completed: membership version and page. Production
    /// keeps a `Placement` cache per worker (`src/app.rs` `Placement::with_memory_budget`), and
    /// its first ranking of a key scores every member, 256 per poll
    /// (`topology/src/placement.rs` `rank_async`, `WORK_QUANTUM`); later rankings of the key
    /// are cache hits.
    ranked: BTreeSet<(u32, u32, u64, Page)>,
    engine: Engine<Ev>,
    trace: TraceHash,
    choices: Choices,
    base: Instant,
    fabric: Fabric,
    origin: Origin,
    /// Published memberships, shared with every core's routing closures.
    versions: Rc<RefCell<Versions>>,
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
    /// Sent probes not yet answered or timed out, by `(node, worker, effect)`.
    probes: BTreeMap<(u32, u32, EffectId), Sent>,
    /// Answered effect ids by `(node, worker)`.
    answered: BTreeSet<(u32, u32, EffectId)>,
    /// Bodies being written, by key.
    bodies: BTreeMap<u64, (u32, u32, Body<Page>)>,
    next_body: u64,
    /// Whether a [`Ev::Tick`] is scheduled.
    ticking: bool,
    /// Whether an [`Ev::Publish`] is scheduled.
    publishing: bool,
    /// `Relay` transfers held by each request's relays, as `(node, worker, reserved at)`.
    relay_charges: BTreeMap<u64, Vec<(u32, u8, Nanos)>>,
    /// `Relay` transfers in use per `(node, worker)`.
    relays_active: BTreeMap<(u32, u8), usize>,
    /// When each running route search of a `(node, worker)` finishes.
    searches: BTreeMap<(u32, u8), Vec<Nanos>>,
    report: Report,
    expected_reads: u64,
    /// Queued [`Ev::ProbeTimeout`], [`Ev::PingTimeout`] and [`Ev::LastHop`] events: bounds that
    /// are no-ops once their exchange ended.
    timeouts_queued: u64,
    /// `MODEL_DIAG` set: record probe, serve and send breakdowns in `attempt_errors`.
    diag: bool,
    /// `MODEL_DIAG`: each acquisition's probe outcomes (`O`wner or `H`elper, answer kind).
    walks: BTreeMap<(u32, u32, ReadId), String>,
    /// Peer requests on their routes.
    msgs: BTreeMap<u64, Msg>,
    next_msg: u64,
    /// Last links awaiting their destination's answer: sender, destination, page, send time
    /// (racer-sim `last_hops`).
    last_hops: BTreeMap<u64, (u32, u32, Page, Nanos)>,
    /// Silence pings in flight by id: requester and destination.
    pings: BTreeMap<u64, (u32, u32)>,
    /// Partitioned node pairs, smaller id first.
    partitions: BTreeSet<(u32, u32)>,
    /// Route searches: the real `topology::Paths` over exhaustive mesh searches, one cache
    /// shared by every node. Production keeps one per worker (`src/app.rs` `Paths::with_limits`)
    /// and racer-sim one per node, but a cache entry's routes depend only on its key, not on the
    /// node that searched (`topology/src/paths.rs` `PathKey` leaves out the seed, which only
    /// picks among the cached alternatives), so sharing changes search counts and CPU, not
    /// which relays carry requests. Per-node caches of production's 128 entries left the 10,000
    /// node cold start unchanged (130 against 134 failures, 0 search sheds) at 18 KB more per
    /// node. Searches run synchronously, so the search queue never fills.
    paths: Paths<Exhaustive>,
}

const TAG_EVENT: u64 = 1;
const TAG_READ_END: u64 = 2;
const TICK: Nanos = 50 * MILLIS;
/// Per-attempt exchange cap (racer-sim `NodeConfig::attempt_timeout_ns`).
pub const ATTEMPT_TIMEOUT: Nanos = 2 * SECONDS;
/// No per-attempt cap: an attempt ends at its overall deadline.
pub const NO_ATTEMPT_CAP: Nanos = Nanos::MAX;
/// A serving read's own scope (racer-sim `NodeConfig::serve_timeout_ns`).
const SERVE_TIMEOUT: Nanos = 5 * SECONDS;
/// Client attempts per read, as in racer-sim.
const MAX_ATTEMPTS: u8 = 4;
/// A link to an unreachable peer fails after this (racer-sim `NodeConfig::connect_timeout_ns`).
const CONNECT_TIMEOUT: Nanos = 100 * MILLIS;
/// Route searches per worker at once (production `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER`
/// default, `src/worker.rs`; racer-sim `NodeConfig::active_searches`).
const ACTIVE_SEARCHES: usize = 8;
/// Entries of the shared route cache.
const ROUTE_CACHE_ENTRIES: usize = 1 << 16;

/// The owners of `page` under `version`, as node ids.
fn owners(placement: &Placement<Phantom>, version: &Version, page: Page) -> Vec<u32> {
    let mut ranked = placement.rank(&version.members, &page.key());
    ranked.truncate(REPLICAS);
    ranked.into_iter().map(|p| version.node(p)).collect()
}

/// The worker of a node that owns `page`'s flight.
fn owner_worker(page: Page, workers: u32) -> u32 {
    (page.object.wrapping_mul(0x9e37_79b9_7f4a_7c15) ^ u64::from(page.number)) as u32 % workers
}

impl<'s> Model<'s> {
    fn new(scenario: &'s Scenario, spec: &'s ModelSpec, choices: Choices) -> Self {
        let n = scenario.nodes.max(1);
        let base = Instant::now();
        let versions = Rc::new(RefCell::new(Versions::new(
            n,
            spec.membership_lag,
            membership::RETAINED,
        )));
        let phantom = Phantom::new();
        let placement = Rc::new(Placement::with_rankings(4096, phantom));
        let fabric = Fabric::new(n, scenario.sites, &scenario.costs, &choices);
        let origin = Origin::new(&scenario.costs, choices.clone());
        let workers = spec.workers.max(1);
        let nodes = (0..n)
            .map(|node| Node {
                cores: (0..workers)
                    .map(|worker| core(node, worker, workers, &versions, &placement, base, spec))
                    .collect(),
                stored: Store::new(spec.stored_pages),
                // Production: half the client request timeout, and the peer attempt timeout for
                // one ping.
                silence: Silence::new(
                    SILENCE_CAPACITY,
                    Duration::from_nanos(spec.read_timeout / 2),
                    Duration::from_nanos(ATTEMPT_TIMEOUT),
                ),
                pings: 0,
                detours: Detours::new(SILENCE_CAPACITY),
                down: false,
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
            versions,
            placement,
            nodes,
            clients: BTreeMap::new(),
            refreshing: BTreeMap::new(),
            ranges: BTreeMap::new(),
            serving: BTreeMap::new(),
            probes: BTreeMap::new(),
            answered: BTreeSet::new(),
            bodies: BTreeMap::new(),
            next_body: 0,
            ticking: false,
            publishing: false,
            relay_charges: BTreeMap::new(),
            relays_active: BTreeMap::new(),
            searches: BTreeMap::new(),
            report: Report::default(),
            expected_reads: 0,
            timeouts_queued: 0,
            diag: std::env::var_os("MODEL_DIAG").is_some(),
            walks: BTreeMap::new(),
            ranked: BTreeSet::new(),
            msgs: BTreeMap::new(),
            next_msg: 0,
            last_hops: BTreeMap::new(),
            partitions: BTreeSet::new(),
            pings: BTreeMap::new(),
            paths: Paths::with_limits(ROUTE_CACHE_ENTRIES, ROUTE_CACHE_ENTRIES * 256, 8),
        }
    }

    /// Whether `node` is listed in the membership it adopted. A node that is not is not ready
    /// (production `/readyz`, `WorkerApplication::membership_usable`), so its clients do not
    /// read through it (racer-sim `reads_skipped_not_member`).
    fn member(&self, node: u32) -> bool {
        let versions = self.versions.borrow();
        versions.adopted(node).position(node).is_some()
    }

    /// The owners of `page` under the latest membership (diagnostics).
    fn latest_owners(&self, page: Page) -> Vec<u32> {
        owners(&self.placement, self.versions.borrow().latest(), page)
    }

    /// The owners of `page` under membership `number`, while it is retained.
    fn owners_at(&self, number: u64, page: Page) -> Option<Vec<u32>> {
        let versions = self.versions.borrow();
        versions
            .get(number)
            .map(|v| owners(&self.placement, v, page))
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

    fn schedule_faults(&mut self, faults: Vec<(Nanos, crate::world::Fault)>) {
        for (at, fault) in faults {
            self.engine.at(at, Ev::Fault(fault));
        }
    }

    /// Whether `node`'s peer server hangs now.
    fn hung(&self, node: u32) -> bool {
        let now = self.now();
        self.spec
            .hangs
            .iter()
            .any(|h| h.node == node && (h.at..h.until).contains(&now))
    }

    /// Whether the link `a - b` is cut: partitioned, or `b` is down.
    fn cut(&self, a: u32, b: u32) -> bool {
        self.nodes[b as usize].down || self.partitions.contains(&(a.min(b), a.max(b)))
    }

    fn schedule_storms(&mut self) {
        let mut churn = self.spec.churn.clone();
        if let Some(random) = self.spec.random_churn {
            let n = self.nodes.len() as u64;
            for _ in 0..random.events {
                let node = self.choices.below(n) as u32;
                let at = self.choices.below(random.within.max(1));
                churn.push(Churn {
                    at,
                    node,
                    join: false,
                });
                churn.push(Churn {
                    at: at + random.away,
                    node,
                    join: true,
                });
            }
        }
        for churn in churn {
            let live = churn.join;
            self.engine.at(
                churn.at,
                Ev::Membership {
                    node: churn.node,
                    live,
                },
            );
        }
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
        self.versions.borrow_mut().now = now;
        match event {
            Ev::Membership { node, live } => {
                if self.versions.borrow_mut().set(node, live) {
                    self.report.counters.membership_changes += 1;
                    if !self.publishing {
                        self.publishing = true;
                        let batch = membership::BATCH;
                        self.engine.at((now / batch + 1) * batch, Ev::Publish);
                    }
                }
            }
            Ev::Refund {
                node,
                worker,
                read,
                remote,
                links,
            } => {
                let core = &mut self.nodes[node as usize].cores[worker as usize];
                // A read that already ended has nothing to refund.
                if let Some(budget) = core.request_budget(read) {
                    let refunded = budget
                        .refund_remote(remote)
                        .and_then(|()| budget.refund_links(links));
                    if let Err(error) = refunded {
                        self.violation(format!("stale refund grew the budget: {error:?}"));
                    }
                    self.report.counters.stale_links_refunded += u64::from(links);
                }
            }
            Ev::Publish => {
                self.publishing = false;
                if self.versions.borrow_mut().publish(now) {
                    self.report.counters.membership_publications += 1;
                }
            }
            Ev::Read {
                node,
                page,
                start,
                attempt,
            } => {
                self.trace.record(now, TAG_EVENT, 1, u64::from(node));
                self.tick_soon();
                if !self.member(node) {
                    self.report.counters.reads_skipped_not_member += 1;
                    return;
                }
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
                if !self.member(node) {
                    let pages = u64::from(self.spec.pages_per_object);
                    self.report.counters.reads_skipped_not_member += pages;
                    return;
                }
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
            Ev::Fault(fault) => {
                use crate::world::Fault as F;
                self.trace.record(now, TAG_EVENT, 11, fault_code(fault));
                match fault {
                    F::Crash { node } => {
                        self.report.counters.crashes += 1;
                        self.nodes[node as usize].down = true;
                    }
                    F::Restart { node } => self.nodes[node as usize].down = false,
                    F::Partition { a, b } if a != b => {
                        self.partitions.insert((a.min(b), a.max(b)));
                    }
                    F::Heal { a, b } => {
                        self.partitions.remove(&(a.min(b), a.max(b)));
                    }
                    F::SlowPeer { node, factor } => self.fabric.slow[node as usize] = factor.max(1),
                    // The Model has no wall or monotonic clock per node.
                    _ => {}
                }
            }
            Ev::Release { msg } => self.release_relays(msg),
            Ev::LinkDown { msg, holder } => self.fail_hop(msg, holder, Fault::Unavailable),
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
                if let Input::Replied { id, reply, .. } = &input {
                    let Some((to, page, _, sent)) = self.probes.remove(&(node, worker, *id)) else {
                        // The attempt timed out first; its late answer is dropped.
                        self.report.counters.late_answers += 1;
                        return;
                    };
                    if let Reply::Answer { .. } = reply {
                        // The requester verifies the signed response on its crypto helper; like
                        // racer-sim (`reply`), the walk does not wait for the slot.
                        let verify = self.fabric.costs.crypto_verify_ns;
                        self.fabric.crypto(now, node, worker as u8, verify);
                    }
                    if self.diag {
                        let owners = self.latest_owners(page);
                        let role = |n: u32| match owners.iter().position(|o| *o == n) {
                            Some(r) => format!("owner{r}"),
                            None => "x".into(),
                        };
                        let kind = match reply {
                            Reply::Answer { kind, signed: true } => format!("{kind:?}"),
                            Reply::Answer {
                                kind,
                                signed: false,
                            } => format!("relay{kind:?}"),
                            other => format!("{other:?}"),
                        };
                        let ms = (now - sent) / (250 * MILLIS) * 250;
                        let key = format!("reply:{}->{}:{kind}:{ms}ms", role(node), role(to));
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                        let walk = self.walks.entry((node, worker, read)).or_default();
                        walk.push(if owners.contains(&to) { 'O' } else { 'H' });
                        walk.extend(kind.chars().take(4));
                        walk.push(' ');
                    }
                }
                let mut out = Vec::new();
                let at = self.instant(now);
                // A stopped acquisition refuses its late answers.
                let _ = self.nodes[node as usize].cores[worker as usize]
                    .on_acquisition(read, input, at, &mut out);
                self.dispatch(node, worker, out);
                self.pump_all(node, worker);
            }
            Ev::Hop { msg, verified } => self.hop(msg, verified),
            Ev::LastHop { msg } => {
                self.timeouts_queued -= 1;
                if let Some((sender, dest, page, sent)) = self.last_hops.remove(&msg) {
                    self.report.counters.silence_timeouts += 1;
                    let (sent, at) = (self.instant(sent), self.instant(now));
                    let known = self.nodes[sender as usize].silence.unanswered(&dest, at);
                    let want = self.nodes[sender as usize].silence.timed_out(
                        dest,
                        page,
                        sent,
                        at,
                        LINK_TIMEOUTS,
                    );
                    if self.diag {
                        let key = format!("lasthop:known{known}:want{want}");
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                    }
                    if want {
                        self.poll_pings(sender);
                    }
                }
            }
            Ev::PingTimeout { ping } => {
                self.timeouts_queued -= 1;
                if let Some((from, to)) = self.pings.remove(&ping) {
                    // Unanswered within the attempt timeout: silent.
                    let answered = racer_core::liveness::ping_verdict(Err(true));
                    self.ping_done(from, to, answered);
                }
            }
            Ev::PingEnd { ping, answered } => {
                if let Some((from, to)) = self.pings.remove(&ping) {
                    self.ping_done(from, to, answered);
                }
            }
            Ev::PollPings { from } => self.poll_pings(from),
            Ev::ProbeTimeout {
                node,
                worker,
                read,
                id,
            } => {
                self.timeouts_queued -= 1;
                if let Some((to, page, copy_only, _)) = self.probes.remove(&(node, worker, id)) {
                    if self.diag {
                        let owners = self.latest_owners(page);
                        let role = match owners.iter().position(|o| *o == to) {
                            Some(r) => format!("owner{r}"),
                            None => "helper".into(),
                        };
                        let me = match owners.iter().position(|o| *o == node) {
                            Some(r) => format!("owner{r}"),
                            None => "reader".into(),
                        };
                        // Does `to` wait on a probe of the same page, and to whom?
                        let waits = self
                            .probes
                            .iter()
                            .filter(|((n, _, _), (_, p, _, _))| *n == to && *p == page)
                            .map(|(_, (t, _, _, _))| {
                                if *t == node {
                                    "back"
                                } else if owners.contains(t) {
                                    "owner"
                                } else {
                                    "other"
                                }
                            })
                            .next()
                            .unwrap_or("none");
                        *self
                            .report
                            .attempt_errors
                            .entry(format!(
                                "timeout:{me}->{role}:copy{copy_only}:waits-{waits}"
                            ))
                            .or_default() += 1;
                    }
                    self.trace.record(now, TAG_EVENT, 10, u64::from(node));
                    self.report.counters.probe_timeouts += 1;
                    let mut out = Vec::new();
                    let at = self.instant(now);
                    let timed_out = Input::Replied {
                        id,
                        reply: Reply::TimedOut,
                        answer: None,
                    };
                    let fed = self.nodes[node as usize].cores[worker as usize]
                        .on_acquisition(read, timed_out, at, &mut out);
                    if self.diag {
                        let silence = out
                            .iter()
                            .filter(|e| matches!(e, Effect::Silence { .. }))
                            .count();
                        let key = format!("probe_timeout_fed:{fed:?}:silence{silence}");
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                    }
                    self.dispatch(node, worker, out);
                    self.pump_all(node, worker);
                }
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
            route_failed: false,
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

    /// Rank `page` for `node` (production `CandidatePolicy::rank_for`, racer-sim `rank_for`).
    /// Polls of `node`'s ranking of `page` on `worker` (racer-sim `spawn`: one
    /// `ranking_chunk_ns` each). A worker's first ranking of a page under a membership scores
    /// every member, `WORK_QUANTUM` (256) per poll; later ones answer from its placement cache
    /// in one poll.
    fn ranking_chunks(
        &mut self,
        node: u32,
        worker: u32,
        page: Page,
        basis: &RankBasis<Ranking, u64>,
    ) -> u64 {
        let number = match basis {
            RankBasis::Verify(ranking) => ranking.0,
            RankBasis::Latest(number) | RankBasis::Fallback(number) => *number,
            RankBasis::Previous { version } => *version,
        };
        let Some(members) = self.versions.borrow().get(number).map(|v| v.count() as u64) else {
            return 1;
        };
        ranking_polls(self.ranked.insert((node, worker, number, page)), members)
    }

    fn rank(
        &self,
        node: u32,
        page: Page,
        basis: RankBasis<Ranking, u64>,
    ) -> Result<RankAnswer<Ranking, u64>, Error> {
        let versions = self.versions.borrow();
        let number = match &basis {
            RankBasis::Verify(ranking) => ranking.0,
            RankBasis::Latest(number) | RankBasis::Fallback(number) => *number,
            RankBasis::Previous { version } => *version,
        };
        let version = versions.get(number).ok_or(Error::IncompatibleMembership)?;
        let owners = owners(&self.placement, version, page);
        if let RankBasis::Previous { version: previous } = basis {
            // Previous owners that are not members of the walked version are not asked.
            let walked = versions.get(previous + 1).ok_or(Error::Unavailable)?;
            let owners: Vec<u32> = owners
                .iter()
                .map(|o| {
                    if walked.position(*o).is_some() {
                        *o
                    } else {
                        NOT_MEMBER
                    }
                })
                .collect();
            let recent = versions.previous_recent_member(node, previous + 1);
            return Ok(RankAnswer {
                ranked: Ranked {
                    recent_member: recent,
                    ..Ranked::new(previous, &owners, None)
                },
                ranking: None,
                recent: recent.then_some(previous),
            });
        }
        if let RankBasis::Verify(initial) = &basis
            && initial.1 != owners
        {
            // Public rankings are not authority.
            return Err(Error::IncompatibleMembership);
        }
        let rank = owners.iter().position(|o| *o == node);
        let mut ranked = Ranked {
            members: version.count(),
            ..Ranked::new(number, &owners, rank)
        };
        if rank.is_none() {
            ranked.order = self.visit_order(node, page, &owners, version);
        }
        Ok(RankAnswer {
            ranked,
            ranking: Some(Box::new((number, owners))),
            recent: None,
        })
    }

    /// A non-owner's visit order over `owners` (production `CandidatePolicy::visit_order`,
    /// racer-sim `walk_ranking`): `Spread` visits mesh neighbors first, then by descending
    /// `replica_score`, so readers spread over the owners instead of all asking rank 0. One
    /// membership version, so `lineage_first` never moves an owner. Owners that left this
    /// node's requests unanswered or shed them go last (`demote_unanswered`).
    fn visit_order(&self, node: u32, page: Page, owners: &[u32], version: &Version) -> VisitOrder {
        let id = |n: u32| NodeId(format!("n{n:08}"));
        let mut key = [0u8; 32];
        key[..8].copy_from_slice(&page.object.to_be_bytes());
        let object = ObjectId {
            cache: CacheId("model".to_owned()),
            key: CacheKey(key),
        };
        let requester = id(node);
        let order = visit_order(
            ReplicaSelection::Spread,
            owners.len(),
            |r| match (version.position(node), version.position(owners[r])) {
                (Some(a), Some(b)) => version.members.is_neighbor(a, b),
                _ => false,
            },
            |r| {
                replica_score(
                    &requester,
                    &object,
                    PageNumber(u64::from(page.number)),
                    &id(owners[r]),
                )
            },
        );
        let at = self.instant(self.now());
        let silence = &self.nodes[node as usize].silence;
        demote_unanswered(order, |r| {
            if silence.silent(&owners[r], at) {
                Answering::Silent
            } else if silence.unanswered(&owners[r], at) {
                Answering::Unanswered
            } else {
                Answering::Answers
            }
        })
    }

    /// Answer `asker`'s probe at node `to`.
    fn serve(&mut self, asker: Asker, to: u32, page: Page, route: Route) {
        let copy_only = route.copy_only;
        if self.diag {
            let owners = self.latest_owners(page);
            if owners.contains(&asker.from) {
                let key = format!(
                    "owner_probe:copy_only{copy_only}:attempts{}:dest_owner{}",
                    route.attempts,
                    owners.contains(&to)
                );
                *self.report.attempt_errors.entry(key).or_default() += 1;
            }
        }
        let now = self.now();
        let worker = owner_worker(page, self.spec.workers.max(1));
        // The destination resolved the request's version (`decide_hop`).
        let (owners, column_mate) = {
            let versions = self.versions.borrow();
            let version = versions.get(route.version).expect("a known version");
            // Production `Membership::same_column` in the default pool (racer-sim
            // `column_mates`): an owner never sheds its own column mates below the exempt cap.
            let column_mate = match (version.position(to), version.position(asker.from)) {
                (Some(a), Some(b)) => version
                    .members
                    .pools()
                    .first()
                    .is_some_and(|pool| pool.same_column(a, b)),
                _ => false,
            };
            (owners(&self.placement, version, page), column_mate)
        };
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
            column_mate,
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
                // Production `inherited_budget` (`src/read.rs`; racer-sim `serve`): the serving
                // read spends the probe's credits within its own serve scope and the route's
                // signed deadline. The probe travelled one hop.
                let scope = self.instant(now + SERVE_TIMEOUT);
                let inherited = inherited_budget(
                    scope,
                    self.instant(route.deadline),
                    route.attempts,
                    route.links,
                    asker.path.len(),
                );
                let budget = match inherited {
                    Ok(budget) => AcquisitionBudgetSpec::from(&budget),
                    Err(error) => {
                        return match peer_error(error) {
                            Some(kind) => self.reply(&asker, to, kind),
                            None => self.fail_probe(&asker, to),
                        };
                    }
                };
                let mut out = Vec::new();
                let core = &mut self.nodes[to as usize].cores[worker as usize];
                let joined = core.acquiring(&page);
                if joined {
                    self.report.counters.flight_joins += 1;
                }
                if self.diag {
                    let own = owners.contains(&to);
                    *self
                        .report
                        .attempt_errors
                        .entry(format!("srv:owner{own}:joined{joined}"))
                        .or_default() += 1;
                }
                let core = &mut self.nodes[to as usize].cores[worker as usize];
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
        if self.diag {
            let (page_bodies, _) = self.nodes[to as usize].cores[worker as usize]
                .serve
                .streaming(&page);
            *self
                .report
                .attempt_errors
                .entry(format!("fanin:{:03}", page_bodies / 8 * 8))
                .or_default() += 1;
        }
        let body = self.nodes[to as usize].cores[worker as usize]
            .serve
            .begin_body(page);
        self.next_body += 1;
        let key = self.next_body;
        self.bodies.insert(key, (to, worker, body));
        // Production drops an exchange its requester abandoned: no chunk starts at or after the
        // requester's attempt bound (racer-sim `transfer_until`, `replies_abandoned`).
        self.answering(&asker, to, now);
        let sealed = self.sign(to, asker.msg);
        let route = Self::back(&asker, to);
        if !self.alive(&route) {
            self.release_relays(asker.msg);
            return self.engine.at(now, Ev::BodyEnd { body: key });
        }
        let end = self
            .fabric
            .transfer_until(sealed, &route, self.spec.page_bytes, asker.abandon);
        self.release_relays_at(asker.msg, end.unwrap_or(asker.abandon));
        let Some(end) = end else {
            self.report.counters.replies_abandoned += 1;
            self.engine
                .at(asker.abandon.max(now), Ev::BodyEnd { body: key });
            return;
        };
        if self.diag {
            let owner = self.latest_owners(page).contains(&to);
            let secs = (end - now) / (MILLIS * 250);
            *self
                .report
                .attempt_errors
                .entry(format!(
                    "send:{}:{}q",
                    if owner { "owner" } else { "helper" },
                    secs
                ))
                .or_default() += 1;
        }
        self.report.counters.peer_bytes += self.spec.page_bytes;
        self.report.counters.peer_pages += 1;
        self.report.counters.peer_bytes += self.spec.page_bytes * (route.len() as u64 - 2);
        if !self
            .owners_at(asker.version, page)
            .is_some_and(|owners| owners.contains(&to))
        {
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

    /// `msg` reached its holder (racer-sim `hop`; production `peer/server.rs` verifies, then
    /// `decide_hop` serves or relays).
    fn hop(&mut self, mid: u64, verified: bool) {
        let now = self.now();
        let Some(m) = self.msgs.get(&mid) else {
            return;
        };
        let at = m.at;
        if self.nodes[at as usize].down {
            // A crash fails the connection: link health, not silence.
            self.report.counters.messages_lost += 1;
            self.last_hops.remove(&mid);
            self.drop_msg(mid);
            return;
        }
        if self.hung(at) {
            // Accepted, never answered or forwarded; the requester's attempt times out.
            self.report.counters.messages_lost += 1;
            self.drop_msg(mid);
            return;
        }
        if now >= m.abandon {
            // The requester gave up: relays and the destination see the upstream connection
            // close and drop the exchange.
            self.report.counters.requests_abandoned += 1;
            self.drop_msg(mid);
            return;
        }
        if !verified {
            // Production verifies every request's signature on the crypto helper
            // (`peer/server.rs` `verify_request_async`) before it dispatches or relays it; the
            // destination's worker then takes it (racer-sim `want`, `request_ns`).
            // The helper is the one paired with the worker holding the connection
            // (`src/worker.rs` `WorkerPair`).
            let costs = self.fabric.costs;
            let conn = self.conn_worker(mid);
            let mut ready = self.fabric.crypto(now, at, conn, costs.crypto_verify_ns);
            if at == m.dest {
                let w = owner_worker(m.page, self.spec.workers.max(1)) as u8;
                ready = self.fabric.cpu(ready, at, w, costs.cpu_request_ns);
            }
            self.engine.at(
                ready,
                Ev::Hop {
                    msg: mid,
                    verified: true,
                },
            );
            return;
        }
        self.trace.record(now, TAG_EVENT, 3, u64::from(at));
        let known = self.versions.borrow().known(at, m.route.version);
        match decide_hop(&at, &m.path, &m.dest, m.route.links, known) {
            Hop::StaleMembership => {
                // Production `peer/server.rs`: a node that does not resolve the request's
                // version answers `StaleMembership`, signed (racer-sim `hop`).
                self.report.counters.stale_membership_replies += 1;
                let asker = self.asker(mid, now);
                self.reply(&asker, at, ResponseKind::StaleMembership);
            }
            Hop::Serve if m.ping.is_some() => {
                // Production mode `ping` is answered `Miss` at once, without looking at the
                // cache or flights (racer-sim `serve`).
                let asker = self.asker(mid, now);
                self.reply(&asker, at, ResponseKind::Miss);
            }
            Hop::Serve => {
                let (page, route) = (m.page, m.route);
                let asker = self.asker(mid, now);
                self.serve(asker, at, page, route);
            }
            Hop::Relay(plan) => {
                if plan.outbound == Err(RouteFault::Invalid) {
                    self.drop_msg(mid);
                    return self.violation(format!("relay {at} received a route revisiting it"));
                }
                // Production `peer/server.rs` reserves one `ResourceClass::Relay` transfer on the
                // relay's worker before it forwards (racer-sim `hop`); none free is `Overloaded`.
                let worker = (mid % u64::from(self.spec.workers.max(1))) as u8;
                let active = self.relays_active.entry((at, worker)).or_default();
                if *active >= self.spec.relay_transfers {
                    self.report.counters.relay_sheds += 1;
                    if self.diag {
                        let m = &self.msgs[&mid];
                        let kind = if m.ping.is_some() { "ping" } else { "probe" };
                        let key = format!("relay_shed:{kind}:hop{}", m.path.len());
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                    }
                    return self.fail_hop(mid, at, Fault::Overloaded);
                }
                *active += 1;
                self.relay_charges
                    .entry(mid)
                    .or_default()
                    .push((at, worker, now));
                self.report.counters.relays += 1;
                self.forward(mid, at, Some(plan));
            }
        }
    }

    /// Drop `mid` without an answer: its relays see the exchange end.
    fn drop_msg(&mut self, mid: u64) {
        self.release_relays(mid);
        self.msgs.remove(&mid);
    }

    /// The relays of `mid` release their `Relay` transfers.
    fn release_relays(&mut self, mid: u64) {
        let now = self.now();
        let kind = if self.diag {
            match self.msgs.get(&mid) {
                Some(m) if m.ping.is_some() => "ping",
                Some(_) => "unanswered",
                None => "answered",
            }
        } else {
            ""
        };
        for (node, worker, start) in self.relay_charges.remove(&mid).unwrap_or_default() {
            if self.diag {
                // Log2 buckets of the hold in ms.
                let ms = (now - start) / MILLIS;
                let bucket = if ms == 0 { 0 } else { 1u64 << ms.ilog2() };
                let key = format!("relay_hold:{kind}:{bucket}ms");
                *self.report.attempt_errors.entry(key).or_default() += 1;
            }
            let key = (node, worker);
            let active = self.relays_active.get_mut(&key).expect("a charged relay");
            *active -= 1;
            if *active == 0 {
                self.relays_active.remove(&key);
            }
        }
    }

    /// The relays of `mid` release their transfers at `at` (production keeps the reservation
    /// on the connection while the body streams, `peer/server.rs` `relay.reservation`).
    fn release_relays_at(&mut self, mid: u64, at: Nanos) {
        if at <= self.now() {
            self.release_relays(mid);
        } else if self.relay_charges.contains_key(&mid) {
            self.engine.at(at, Ev::Release { msg: mid });
        }
    }

    /// Take `mid` off its route: whoever answers it now answers its requester.
    fn asker(&mut self, mid: u64, now: Nanos) -> Asker {
        let m = self.msgs.remove(&mid).expect("message");
        Asker {
            from: m.from,
            worker: m.worker,
            read: m.read,
            id: m.id,
            at: now,
            abandon: m.abandon,
            path: m.path,
            dest: m.dest,
            msg: mid,
            version: m.route.version,
            attempts: m.route.attempts,
            links: m.route.charged,
            ping: m.ping,
        }
    }

    /// Route `msg` one link from `holder` (racer-sim `forward`; production `Paths::shortest`
    /// with `LinkHealth` blocks and `Paths::detour`). `plan` is the relay plan at a relay.
    fn forward(&mut self, mid: u64, holder: u32, plan: Option<RelayPlan>) {
        let now = self.now();
        let at = self.instant(now);
        let m = &self.msgs[&mid];
        let dest = m.dest;
        // Production `shortest_probe_async`: a requester's own ping may use the link to the
        // destination it holds silent, and always detours past the primary relay.
        let own_ping = m.ping.is_some() && holder == m.from;
        if !own_ping && self.nodes[holder as usize].silence.silent(&dest, at) {
            // The destination is a silent next hop of this node.
            return self.fail_hop(mid, holder, Fault::Unavailable);
        }
        let search = match plan {
            None => search_budget(&m.path, m.route.links, &holder),
            Some(plan) => plan.search,
        };
        let Ok((keep, links)) = search else {
            return self.fail_hop(mid, holder, Fault::HopBudgetExhausted);
        };
        let Some(members) = self
            .versions
            .borrow()
            .get(m.route.version)
            .map(|v| v.members.clone())
        else {
            // racer-sim `forward`: the request's version is no longer resolvable here.
            return self.fail_hop(mid, holder, Fault::IncompatibleMembership);
        };
        let position = |n: u32| members.position(&n.to_be_bytes());
        let (Some(from), Some(to)) = (position(holder), position(dest)) else {
            return self.fail_hop(mid, holder, Fault::Unavailable);
        };
        let Some(visited) = m.path[..keep]
            .iter()
            .map(|n| position(*n))
            .collect::<Option<Vec<usize>>>()
        else {
            return self.fail_hop(mid, holder, Fault::Unavailable);
        };
        let node = &self.nodes[holder as usize];
        let mut blocked: Vec<usize> = node
            .silence
            .silent_peers(at)
            .filter(|p| !(own_ping && **p == dest))
            .filter_map(|p| position(*p))
            .filter(|p| members.is_neighbor(from, *p))
            .collect();
        blocked.sort_unstable();
        let mut level = node.detours.level(&dest, at);
        if own_ping {
            level = level.max(1);
        }
        blocked.extend(members.detour(from, to, level));
        let seed = mid.to_be_bytes();
        let query = PathQuery {
            from,
            to,
            links,
            visited: &visited,
            blocked: &blocked,
            seed: &seed,
        };
        let worker = (mid % u64::from(self.spec.workers.max(1))) as u8;
        // Production (`src/app.rs` `Paths::with_limits`) runs at most
        // `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER` (8) searches per worker and queues none: a
        // search past them fails `Overloaded` (`topology::Paths::route` with no queue). A slot
        // is held until the search's CPU work is done.
        let running = {
            let ends = self.searches.entry((holder, worker)).or_default();
            ends.retain(|end| *end > now);
            ends.len()
        };
        let (route, polls) = self.route(&members, query, || running < ACTIVE_SEARCHES);
        // racer-sim `spawn`: a cache hit answers on the first poll, each further poll expands
        // up to 32 nodes, on one worker's CPU.
        let costs = self.fabric.costs;
        let cost = polls.saturating_sub(1) * 32 * costs.cpu_path_expand_ns_per_node;
        let ready = self.fabric.cpu(now, holder, worker, cost);
        if polls > 1 && route != Err(topology::Error::Overloaded) {
            self.searches
                .entry((holder, worker))
                .or_default()
                .push(ready);
        }
        if polls > 1 && route == Err(topology::Error::Overloaded) {
            self.report.counters.search_sheds += 1;
        }
        let route = match route {
            Ok(route) => route,
            Err(topology::Error::Overloaded) => {
                return self.fail_hop(mid, holder, Fault::Overloaded);
            }
            Err(_) => {
                self.report.counters.route_unreachable += 1;
                return self.fail_hop(mid, holder, Fault::Unavailable);
            }
        };
        let next = u32::from_be_bytes(members.members()[route[1]].0);
        self.link(mid, holder, plan, next, ready);
    }

    /// Run one route search to completion; returns the route and the polls it took. A cache
    /// hit answers on the first poll; a search that has to run needs one of the holder worker's
    /// search slots (`admit`), or fails `Overloaded`.
    fn route(
        &self,
        members: &Members,
        query: PathQuery<'_>,
        admit: impl FnOnce() -> bool,
    ) -> (Result<Vec<usize>, topology::Error>, u64) {
        let mut future = std::pin::pin!(self.paths.route(members, query));
        let mut cx = std::task::Context::from_waker(std::task::Waker::noop());
        let mut admit = Some(admit);
        let mut polls = 0;
        loop {
            polls += 1;
            if let std::task::Poll::Ready(route) = future.as_mut().poll(&mut cx) {
                return (route, polls);
            }
            if admit.take().is_some_and(|admit| !admit()) {
                // Dropping the future cancels the search.
                return (Err(topology::Error::Overloaded), polls);
            }
            // Searches run one at a time, so a search never waits on another.
            assert!(polls < 1_000, "a route search did not finish");
        }
    }

    /// Send `msg` from `holder` over its link to `next` once `ready` (racer-sim `link`).
    fn link(&mut self, mid: u64, holder: u32, plan: Option<RelayPlan>, next: u32, ready: Nanos) {
        let now = self.now();
        let m = self.msgs.get_mut(&mid).expect("message");
        if let Some(plan) = plan {
            let Ok(outbound) = plan.outbound else {
                return self.fail_hop(mid, holder, Fault::HopBudgetExhausted);
            };
            m.path.push(holder);
            m.route.links = outbound;
        }
        m.at = next;
        if self.cut(holder, next) {
            self.report.counters.link_failures += 1;
            let failed = Ev::LinkDown { msg: mid, holder };
            return self.engine.at(now + CONNECT_TIMEOUT, failed);
        }
        let m = self.msgs.get_mut(&mid).expect("message");
        if next == m.dest {
            // The last link: this sender learns whether the destination answers.
            self.last_hops.insert(mid, (holder, next, m.page, now));
            let abandon = m.abandon.max(now + 1);
            self.timeouts_queued += 1;
            self.engine.at(abandon, Ev::LastHop { msg: mid });
        }
        let conn = if holder == self.msgs[&mid].from {
            self.msgs[&mid].worker as u8
        } else {
            self.conn_worker(mid)
        };
        let signed = self
            .fabric
            .crypto(ready, holder, conn, self.fabric.costs.crypto_sign_ns);
        let arrive = self.fabric.message(signed, holder, next);
        self.engine.at(
            arrive,
            Ev::Hop {
                msg: mid,
                verified: false,
            },
        );
    }

    /// Forwarding `msg` from `holder` failed with `fault` (racer-sim `fail_hop`; production
    /// `relay_failure`).
    fn fail_hop(&mut self, mid: u64, holder: u32, fault: Fault) {
        let Some(m) = self.msgs.remove(&mid) else {
            return;
        };
        let asker = Asker {
            from: m.from,
            worker: m.worker,
            read: m.read,
            id: m.id,
            at: self.now(),
            abandon: m.abandon,
            path: m.path,
            dest: m.dest,
            msg: mid,
            version: m.route.version,
            attempts: m.route.attempts,
            links: m.route.charged,
            ping: m.ping,
        };
        if holder == asker.from && asker.path.len() == 1 {
            // The requester's own send failed: a transport error. A shed route search fails as
            // production `request_network` (`src/peer.rs`) does, with `Overloaded`, which is no
            // evidence against the destination.
            if let Some(ping) = asker.ping {
                let now = self.now();
                return self.end_ping(ping, now, Err(false));
            }
            let error = if fault == Fault::Overloaded {
                Error::Overloaded
            } else {
                Error::Unavailable
            };
            let failed = Input::Replied {
                id: asker.id,
                reply: Reply::Failed {
                    error,
                    sent: true,
                    scope_live: true,
                },
                answer: None,
            };
            let input = Ev::Input {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                input: failed,
            };
            return self.engine.at(self.now(), input);
        }
        self.report.counters.relay_failures += 1;
        match relay_failure(fault, asker.ping.is_some()) {
            RelayFailure::Respond(r) => self.reply(&asker, holder, response_kind(r)),
            RelayFailure::RespondIfLive(r) if self.now() < m.route.deadline => {
                self.reply(&asker, holder, response_kind(r));
            }
            RelayFailure::RespondIfLive(_) => self.release_relays(mid),
            RelayFailure::Propagate => self.fail_probe(&asker, holder),
        }
    }

    /// The route back from `from` to `asker`: `from`, then the request's path reversed.
    fn back(asker: &Asker, from: u32) -> Vec<u32> {
        let mut route = Vec::with_capacity(asker.path.len() + 1);
        route.push(from);
        route.extend(asker.path.iter().rev().copied());
        route
    }

    /// `from` answers `asker`, ready at `ready`: an answer from the destination before the
    /// requester gave up clears its last link (racer-sim `send_reply`). Returns whether the
    /// answer is the destination's own (signed by it).
    fn answering(&mut self, asker: &Asker, from: u32, ready: Nanos) -> bool {
        let own = from == asker.dest;
        if own
            && ready < asker.abandon
            && let Some((sender, dest, ..)) = self.last_hops.remove(&asker.msg)
        {
            let at = self.instant(self.now());
            self.nodes[sender as usize].silence.answered(&dest, at);
        }
        own
    }

    /// Whether every link of `route` is up (racer-sim `send_reply`: a reply over a cut link is
    /// lost and the requester times out).
    fn alive(&mut self, route: &[u32]) -> bool {
        let alive = !self.nodes[route[0] as usize].down
            && route.windows(2).all(|hop| !self.cut(hop[0], hop[1]));
        if !alive {
            self.report.counters.replies_lost += 1;
        }
        alive
    }

    /// A message from `asker`'s replier back over `route`, leaving at `ready`.
    fn message_back(&self, ready: Nanos, route: &[u32]) -> Nanos {
        route
            .windows(2)
            .fold(ready, |t, hop| self.fabric.message(t, hop[0], hop[1]))
    }

    /// `node` signs a peer response on its crypto helper (production `sign_response` in
    /// `peer/server.rs`; racer-sim `send_reply`); returns when the signature is ready.
    /// The helper is the one paired with the worker holding `mid`'s connection.
    fn sign(&mut self, node: u32, mid: u64) -> Nanos {
        let now = self.now();
        let conn = self.conn_worker(mid);
        self.fabric
            .crypto(now, node, conn, self.fabric.costs.crypto_sign_ns)
    }

    /// The worker of a node that holds request `mid`'s connection, as the Model spreads peer
    /// connections over workers (racer-sim `hop`: `probe % workers`). An inference: production
    /// assigns accepted connections to workers by its listener, not by request.
    fn conn_worker(&self, mid: u64) -> u8 {
        (mid % u64::from(self.spec.workers.max(1))) as u8
    }

    /// Send `from`'s due silence pings, at most [`MAX_PINGS_IN_FLIGHT`] at once (production
    /// `CandidatePolicy::poll_pings` in `src/read/candidates.rs`; racer-sim `poll_pings`). A ping
    /// is a peer request (`FetchMode::Ping`) routed over the mesh like any other, on a fresh
    /// grant: no remote attempts, [`NOTICE_LINKS`] links and the peer attempt timeout
    /// (production `CandidatePolicy::ping` through `request_traced`; racer-sim
    /// `launch_control`).
    fn poll_pings(&mut self, from: u32) {
        let now = self.now();
        let at = self.instant(now);
        let n = &mut self.nodes[from as usize];
        let room = MAX_PINGS_IN_FLIGHT.saturating_sub(n.pings);
        if room == 0 {
            return;
        }
        for (to, page) in n.silence.due_pings(at, room) {
            self.nodes[from as usize].pings += 1;
            self.report.counters.silence_pings += 1;
            self.next_msg += 1;
            let mid = self.next_msg;
            self.pings.insert(mid, (from, to));
            let timeout = now + ATTEMPT_TIMEOUT;
            let version = self.versions.borrow().adopted(from).number;
            let msg = Msg {
                from,
                worker: 0,
                read: ReadId(0),
                id: EffectId::default(),
                dest: to,
                page,
                route: Route {
                    version,
                    copy_only: true,
                    deadline: timeout,
                    attempts: 0,
                    links: NOTICE_LINKS,
                    charged: NOTICE_LINKS,
                },
                path: vec![from],
                at: from,
                abandon: timeout,
                ping: Some(mid),
            };
            self.msgs.insert(mid, msg);
            self.timeouts_queued += 1;
            self.engine.at(timeout, Ev::PingTimeout { ping: mid });
            self.forward(mid, from, None);
        }
    }

    /// Ping `ping` ends at `at` with the verdict on `answer` (`ping_verdict`).
    fn end_ping(&mut self, ping: u64, at: Nanos, answer: Result<(bool, bool), bool>) {
        let answered = racer_core::liveness::ping_verdict(answer);
        self.engine.at(at, Ev::PingEnd { ping, answered });
    }

    /// `from`'s ping to `to` ended (racer-sim `ping_done`; production `LinkHealth::ping_outcome`).
    fn ping_done(&mut self, from: u32, to: u32, answered: Option<bool>) {
        let at = self.instant(self.now());
        let n = &mut self.nodes[from as usize];
        n.pings -= 1;
        if answered.is_some() {
            n.silence.ping_result(&to, answered, at);
            if n.silence.silent(&to, at) {
                // Ask again after the hold to learn that it recovered.
                let hold = n.silence.hold().as_nanos() as Nanos;
                self.engine.after(hold, Ev::PollPings { from });
            }
        }
        self.poll_pings(from);
    }

    /// The serving read on `to` failed without a peer response: a transport failure.
    fn fail_probe(&mut self, asker: &Asker, to: u32) {
        self.release_relays(asker.msg);
        let now = self.now();
        self.answering(asker, to, now);
        let route = Self::back(asker, to);
        if !self.alive(&route) {
            return;
        }
        let at = self.message_back(now, &route);
        if let Some(ping) = asker.ping {
            // A transport failure proves nothing.
            return self.end_ping(ping, at, Err(false));
        }
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
        self.release_relays(asker.msg);
        let own = self.answering(asker, to, self.now());
        let route = Self::back(asker, to);
        if !self.alive(&route) {
            return;
        }
        let ready = self.sign(to, asker.msg);
        let at = self.message_back(ready, &route);
        if let Some(ping) = asker.ping {
            // A signed answer: alive when the peer itself answered, silent when a relay
            // answered `Unavailable` (production `CandidatePolicy::ping`).
            return self.end_ping(ping, at, Ok((own, kind == ResponseKind::Unavailable)));
        }
        if kind == ResponseKind::StaleMembership {
            // Production `request_traced`: the answering node could not resolve the request's
            // membership, so the remote credits it carried were never used, and neither were the
            // links past the hops that carried it there (racer-sim `stale_links_refunded`).
            let refund = Ev::Refund {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                remote: asker.attempts,
                links: stale_unused_links(asker.links, asker.path.len()),
            };
            self.engine.at(at, refund);
        }
        self.engine.at(
            at,
            Ev::Input {
                node: asker.from,
                worker: asker.worker,
                read: asker.read,
                input: Input::Replied {
                    id: asker.id,
                    // A relay's answer is not the destination's signed answer.
                    reply: Reply::Answer { kind, signed: own },
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
                Acq::Rank { id, basis, .. } => {
                    let chunks = self.ranking_chunks(node, worker, page, &basis);
                    let at = self.fabric.cpu(
                        now,
                        node,
                        worker as u8,
                        chunks * costs.cpu_ranking_chunk_ns,
                    );
                    let result = self.rank(node, page, basis);
                    self.engine
                        .at(at, input(read, Input::Ranked { id, result }));
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
                Acq::Refresh { id, version } => {
                    // racer-sim `Action::Refresh`: the newest version this node adopted, and
                    // version - 1 while it is recent and lists this node.
                    let versions = self.versions.borrow();
                    let latest = versions.adopted(node).number;
                    let recent_previous = versions
                        .previous_recent_member(node, version)
                        .then(|| version - 1);
                    drop(versions);
                    let answer = Input::Membership {
                        id,
                        latest: Ok(Some((latest, latest))),
                        recent_previous,
                    };
                    self.engine
                        .at(now + costs.cpu_request_ns, input(read, answer));
                }
                Acq::Send {
                    id,
                    probe,
                    dest,
                    version,
                } => {
                    self.report.counters.peer_probes += 1;
                    let Some(to) = dest.filter(|d| (*d as usize) < self.nodes.len()) else {
                        self.probes
                            .insert((node, worker, id), (node, page, false, now));
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
                    // Production `request_traced` (racer-sim `probe`): reserve the route and
                    // the remote credits on the read's request budget, then bound the exchange.
                    let at_now = self.instant(now);
                    let Some(budget) =
                        self.nodes[node as usize].cores[worker as usize].request_budget(read)
                    else {
                        return self.violation(format!("node {node} sent for unknown {read:?}"));
                    };
                    let caller_deadline = budget.deadline();
                    let plan = plan_attempt(
                        budget,
                        AttemptInputs {
                            now: at_now,
                            caller_deadline,
                            remaining_opportunities: probe.remaining_opportunities,
                            route: probe.route,
                            mode: probe.mode,
                            attempt_timeout: Duration::from_nanos(self.spec.attempt_timeout),
                            failure_links: MAX_LINKS,
                        },
                    );
                    let copy_only = probe.mode == racer_core::read::walk::Mode::CopyOnly;
                    self.probes
                        .insert((node, worker, id), (to, page, copy_only, now));
                    let plan = match plan {
                        Ok(plan) => plan,
                        Err(error) => {
                            let failed = Input::Replied {
                                id,
                                reply: Reply::Failed {
                                    error,
                                    sent: false,
                                    scope_live: false,
                                },
                                answer: None,
                            };
                            self.engine.at(now, input(read, failed));
                            return;
                        }
                    };
                    let deadline = self.nanos(plan.overall);
                    let timeout = self.nanos(plan.attempt_end).max(now + 1);
                    self.timeouts_queued += 1;
                    self.engine.at(
                        timeout,
                        Ev::ProbeTimeout {
                            node,
                            worker,
                            read,
                            id,
                        },
                    );
                    self.next_msg += 1;
                    let mid = self.next_msg;
                    let msg = Msg {
                        from: node,
                        worker,
                        read,
                        id,
                        dest: to,
                        page,
                        route: Route {
                            version,
                            copy_only,
                            deadline,
                            attempts: plan.remote_attempts,
                            links: plan.links,
                            charged: plan.links,
                        },
                        path: vec![node],
                        at: node,
                        abandon: timeout,
                        ping: None,
                    };
                    self.msgs.insert(mid, msg);
                    self.forward(mid, node, None);
                }
                Acq::Validate { id, .. } => {
                    let at = self.fabric.crypto(
                        now,
                        node,
                        worker as u8,
                        costs.open(self.spec.page_bytes),
                    );
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
                    self.report.counters.stale_waits += 1;
                    let at = self.nanos(until).max(now);
                    self.engine
                        .at(at, input(read, Input::Timer { id, result: Ok(()) }));
                }
                Acq::Origin { id, .. } => {
                    self.report.counters.origin_requests += 1;
                    *self.report.origin_per_page.entry(page).or_default() += 1;
                    if self.diag {
                        // Which owner asked origin, after what walk.
                        let owners = self.latest_owners(page);
                        let rank = owners.iter().position(|o| *o == node);
                        let walk = self.walks.get(&(node, worker, read)).cloned();
                        let key = format!("origin:rank{rank:?}:{}", walk.unwrap_or_default());
                        *self.report.attempt_errors.entry(key).or_default() += 1;
                    }
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
            Effect::Silence {
                page,
                dest,
                sent,
                note,
            } => {
                // Production `CandidatePolicy::apply` (racer-sim `apply`, `shed_busy` on).
                let at = self.instant(now);
                let silence = &mut self.nodes[node as usize].silence;
                match note {
                    SilenceNote::Shed => silence.shed(dest, page, at),
                    SilenceNote::Answered => silence.answered(&dest, at),
                    SilenceNote::TimedOut { needed } => {
                        // Production `LinkHealth::silence_timed_out`: later routes to `dest`
                        // skip the relay that may have eaten this exchange.
                        self.nodes[node as usize].detours.timed_out(dest, sent, at);
                        if self.diag {
                            let key = format!(
                                "silence_timeout:needed{}:waited{}ms",
                                needed.min(9),
                                at.saturating_duration_since(sent).as_millis() / 500 * 500
                            );
                            *self.report.attempt_errors.entry(key).or_default() += 1;
                        }
                        let silence = &mut self.nodes[node as usize].silence;
                        if silence.timed_out(dest, page, sent, at, needed) {
                            self.poll_pings(node);
                        }
                    }
                }
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
            if self.diag {
                let owners = self.latest_owners(page);
                let me = match owners.iter().position(|o| *o == node) {
                    Some(r) => format!("owner{r}"),
                    None => "helper".into(),
                };
                let ms = (now - asker.at) / (250 * MILLIS) * 250;
                let r = match result {
                    Ok(()) => "ok".to_string(),
                    Err(e) => format!("{e:?}"),
                };
                *self
                    .report
                    .attempt_errors
                    .entry(format!("served:{me}:{r}:{ms}ms"))
                    .or_default() += 1;
            }
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
        for walk in std::mem::take(&mut self.walks).into_values() {
            if !walk.ends_with("Page ") {
                *self
                    .report
                    .attempt_errors
                    .entry(format!("walk:{walk}"))
                    .or_default() += 1;
            }
        }
        if !self.msgs.is_empty() {
            self.violation(format!("{} peer requests never ended", self.msgs.len()));
        }
        if !self.relay_charges.is_empty() {
            let held = self.relay_charges.len();
            self.violation(format!("{held} requests still hold relay transfers"));
        }
        let c = &self.report.counters;
        let ended = c.reads_completed + c.reads_failed + c.reads_skipped_not_member;
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
                self.report.counters.flights_reelected += observed.reelected;
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
        Effect::Silence { .. } => "Silence",
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
    versions: &Rc<RefCell<Versions>>,
    placement: &Rc<Placement<Phantom>>,
    base: Instant,
    spec: &ModelSpec,
) -> WorkerCore<ModelTypes> {
    // Every closure reads the version this node adopted when the core calls it.
    let (v, p) = (versions.clone(), placement.clone());
    let plan = Box::new(move |page: &Page, budget: &AcquisitionBudgetSpec| {
        let config = FillConfig {
            operation: OperationKind::Page,
            hedge: false,
            walk_only: false,
            scope: budget.deadline,
            features: Features::PRODUCTION,
        };
        let versions = v.borrow();
        let version = versions.adopted(node);
        (
            config,
            Box::new((version.number, owners(&p, version, *page))),
        )
    });
    let (v, p) = (versions.clone(), placement.clone());
    let mesh = Box::new(move |page: &Page, version: u64| {
        let versions = v.borrow();
        Some(Mesh {
            owners: owners(&p, versions.get(version)?, *page),
            start: helper_start(&node.to_be_bytes()),
            reader: node,
        })
    });
    let v = versions.clone();
    let helper_of = Box::new(move |owner: u32| {
        let versions = v.borrow();
        let version = versions.adopted(node);
        let pool = version.members.pools().first()?;
        let helper = pool.helper(version.position(node)?, version.position(owner)?)?;
        Some(version.node(helper))
    });
    WorkerCore::new(
        Routing {
            me: worker,
            owner_of: Box::new(move |page| owner_worker(*page, workers)),
            plan,
            mesh,
            helper_of,
        },
        FlightLimits {
            waiters_per_flight: spec.waiters_per_flight,
            ..flight_limits()
        },
        64,
        ServeDecision::new(1 << 20),
        SuccessorHints::new(HINT_CAPACITY, HINT_TTL),
        OwnerBudget::new(4, 0),
        base,
    )
}

/// Production `Relay` transfers per worker (`racer_core::admission::Limits::default`
/// `relay_transfers`, racer-sim `NodeConfig::relay_transfers`).
fn relay_transfers() -> usize {
    racer_core::admission::Limits::default()
        .relay_transfers
        .get()
}

/// Production per-worker flight bounds (`racer_core::admission::Limits::default`, racer-sim
/// `NodeConfig`): 64 flights of 64 waiters each. A join past the entry cap is answered
/// `Overloaded`; past a flight's waiter cap, `FlightFull`.
fn flight_limits() -> FlightLimits {
    let limits = racer_core::admission::Limits::default();
    FlightLimits {
        entries: limits.flights.get(),
        waiters_per_flight: limits.waiters_per_flight.get(),
        ..FlightLimits::default()
    }
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
    // Draw a fault schedule only when the scenario asks for one, so fault-free scenarios keep
    // their choice streams.
    let faults = if scenario.fault_mix.max_faults == 0 {
        scenario.faults.clone()
    } else {
        scenario.fault_schedule(&choices)
    };
    model.schedule_faults(faults);
    model.schedule_storms();
    model.run(scenario.end, scenario.max_steps);
    // A probe answered before its bound leaves its timeout queued; it would be a no-op.
    let quiescent =
        model.engine.pending() as u64 == model.timeouts_queued && model.probes.is_empty();
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

/// A trace code for `fault`.
fn fault_code(fault: crate::world::Fault) -> u64 {
    use crate::world::Fault as F;
    match fault {
        F::Crash { node } => u64::from(node),
        F::Restart { node } => 1 << 32 | u64::from(node),
        F::Partition { a, b } => 2 << 32 | u64::from(a) << 16 | u64::from(b),
        F::Heal { a, b } => 3 << 32 | u64::from(a) << 16 | u64::from(b),
        F::SlowPeer { node, factor } => 4 << 32 | u64::from(factor) << 16 | u64::from(node),
        _ => 5 << 32,
    }
}

/// Polls of one ranking over `members` (production `topology/src/placement.rs` `rank_async`):
/// a first ranking scores `WORK_QUANTUM` (256) members per poll; a cached one takes one poll.
fn ranking_polls(first: bool, members: u64) -> u64 {
    const WORK_QUANTUM: u64 = 256;
    if first {
        members.div_ceil(WORK_QUANTUM).max(1)
    } else {
        1
    }
}

/// The response kind a relay signs for `r` (racer-sim `response_kind`).
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_first_ranking_scores_every_member_and_a_cached_one_polls_once() {
        assert_eq!(ranking_polls(true, 0), 1);
        assert_eq!(ranking_polls(true, 256), 1);
        assert_eq!(ranking_polls(true, 257), 2);
        assert_eq!(ranking_polls(true, 50_000), 196);
        assert_eq!(ranking_polls(false, 50_000), 1);
    }

    fn page(number: u32) -> Page {
        Page { object: 0, number }
    }

    #[test]
    fn a_rank0_timeout_pings_and_an_answer_clears_the_silence() {
        let scenario = crate::scenarios::cold_start(16);
        let spec = scenario.model.as_deref().expect("model spec");
        let mut model = Model::new(&scenario, spec, Choices::seeded(1));
        let sent = model.instant(0);
        model.engine.at(SECONDS, Ev::PollPings { from: 0 });
        model.run(SECONDS, u64::MAX);
        let at = model.instant(model.now());
        let needed = racer_core::liveness::RANK0_TIMEOUTS;
        for _ in 0..needed {
            model.nodes[0]
                .silence
                .timed_out(5, page(1), sent, at, needed);
        }
        assert!(model.nodes[0].silence.unanswered(&5, at));
        model.poll_pings(0);
        assert_eq!(model.report.counters.silence_pings, 1);
        assert_eq!(model.nodes[0].pings, 1);
        model.run(2 * SECONDS, u64::MAX);
        assert_eq!(model.nodes[0].pings, 0);
        let at = model.instant(model.now());
        assert!(!model.nodes[0].silence.unanswered(&5, at));
        assert!(!model.nodes[0].silence.silent(&5, at));
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
