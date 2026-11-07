//! Model fidelity: a byte-free interpreter over the real
//! [`WorkerCore`] machines.
//!
//! Every node runs `workers` real [`WorkerCore`]s and a real [`Placement`] over
//! [`phantom::Phantom`] rankings. The interpreter is dispatch-only: it completes each
//! effect the cores emit on the shared FIFO cost model ([`crate::resources`]), priced from
//! `costs.toml`, as an [`Engine`] event, and answers it with the facts of the world (which
//! node holds which page). It never decides where to probe, when to fetch from origin or
//! whom to shed; the cores do. See `dst/MODEL.md` for what is and is not modeled.

mod client;
mod combined;
mod completion;
pub mod control;
pub mod coverage;
#[allow(missing_docs)]
pub mod ctl_report;
mod driver;
mod events;
pub mod membership;
pub mod phantom;
mod probes;
mod repeats;
mod report;
mod serve;
mod setup;
mod spec;
#[cfg(test)]
mod tests;
mod transport;

use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet},
    rc::Rc,
    time::{Duration, Instant},
};

pub use racer_core::connections::Config as ConnectionConfig;
use racer_core::{
    connections::{Acquire, ConnId, Connections},
    flow::Circuits,
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
        Fault, Hop, MAX_LINKS, RelayFailure, RelayPlan, Response, RouteFault, UnverifiedFailure,
        decide_hop, relay_failure, search_budget, unverified_failure,
    },
};
use racer_wire::control::{CacheId, NodeId};
use racer_wire::object::model::{CacheKey, ObjectId, PageNumber};
use serde_json::Value;
use topology::{Exhaustive, PathQuery, Paths, Placement, REPLICAS};

pub use report::{Counters, Report};

use racer_core::time::SplitMix64;

use crate::{
    choices::Choices,
    choices::mix as mix64,
    engine::{Engine, MILLIS, Nanos, SECONDS, TraceHash},
    invariants::Class,
    resources::{Fabric, Origin, OriginErrors, OriginOutcome},
    scenario::Scenario,
    world::{Failure, Outcome},
};
use driver::*;
pub use driver::{effect_name, run};
use membership::{Members, Version, Versions};
use phantom::Phantom;
use repeats::{Asked, Cut};
pub use spec::*;

type Input = acquisition::Input<Ranking, u64, u32>;

/// Engine events.
enum Ev {
    /// The next [`Workload`] read of `node`.
    Arrival { node: u32 },
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
    /// The destination verified request `msg`; its worker takes it next.
    Verified { msg: u64 },
    /// The requester of `msg` gave up: a last link still unanswered is a timeout for its
    /// sender.
    LastHop { msg: u64 },
    /// The requester of `msg` gave up: it closes its connection and the relays see theirs close
    /// (`Model::conns`).
    Abandon { msg: u64 },
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
    LinkDown {
        msg: u64,
        holder: u32,
        worker: u8,
        next: u32,
    },
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

/// A peer request on its route (production `peer/forwarding.rs`).
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
    /// Production `LinkHealth` silence, shared by the node's workers: fed by
    /// [`Effect::Silence`], read by every non-owner ranking.
    silence: Silence<u32, Page>,
    /// Silence pings in flight (production `CandidatePolicy::pings`).
    pings: usize,
    /// Production `LinkHealth` detours: first hops a route
    /// skips after timeouts.
    detours: Detours<u32>,
    /// Crashed and not yet restarted: its peer server takes no connections.
    down: bool,
    /// When it last crashed.
    down_at: Nanos,
}

/// When an acquisition started, and how its walk probes ended.
type Walked = (Nanos, Vec<(u32, Asked)>);

/// One worker's link circuits (production `LinkHealth::inner`, `src/topology.rs`).
struct Links {
    circuits: Circuits<u32>,
    /// Peers with a failure record, for route searches' blocked neighbors (production
    /// `Paths::blocked` asks the circuit of every mesh neighbor).
    failed: BTreeSet<u32>,
}

impl Links {
    fn new() -> Self {
        Self {
            // Production `LINK_HEALTH_CAPACITY` and a 1 s half-open probe timeout.
            circuits: Circuits::new(LINK_HEALTH_CAPACITY, Duration::from_secs(1)),
            failed: BTreeSet::new(),
        }
    }
}

/// How a walk probe's reply ended, for repeat classification.
fn asked(reply: &Reply, cut: Option<Cut>) -> Asked {
    use racer_core::read::ResponseKind as K;
    match *reply {
        Reply::Answer { kind, .. } if kind.has_payload() => Asked::Copy,
        Reply::Answer {
            kind: K::Miss | K::VersionUnavailable,
            ..
        } => Asked::Miss,
        Reply::Answer {
            kind: K::Unavailable,
            signed,
        } => {
            if signed {
                Asked::Failed
            } else {
                Asked::Relayed
            }
        }
        Reply::Answer {
            kind: K::Overloaded | K::FlightFull | K::StaleMembership,
            ..
        } => Asked::Busy,
        Reply::Answer { .. } => Asked::Unfunded,
        Reply::TimedOut => Asked::Unreachable(cut),
        Reply::Failed {
            error: Error::Overloaded,
            ..
        } => Asked::Busy,
        Reply::Failed { .. } => Asked::Unreachable(cut),
    }
}

/// Production `LINK_HEALTH_CAPACITY` (`src/topology.rs`).
const LINK_HEALTH_CAPACITY: usize = 4096;

/// Production link backoff (`racer_core::route::link_backoff`); the jitter is a fixed mix of
/// the peer and failure count instead of SHA-256 of the node name
fn link_backoff(node: &u32, failures: u32) -> Duration {
    racer_core::route::link_backoff(
        failures,
        mix64((u64::from(*node) << 32) | u64::from(failures)),
    )
}

/// A node's published pages, bounded by least-recently-used eviction (the Model does not
/// model the production disk index).
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
    /// [`Workload`] draws and its Zipf CDF.
    workload_rng: SplitMix64,
    zipf: Vec<f64>,
    /// No [`Workload`] read starts after this, so every read ends by the scenario's end.
    arrivals_until: Nanos,
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
    /// The admitted body request of each serving read (production `dispatch_verified` holds a
    /// `Streaming` from the hot-page check until the response is ready), counted toward the
    /// page's hot-page fan-in while it waits.
    admitted: BTreeMap<(u32, u32, RequestId), racer_core::read::serve::Body<Page>>,
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
    /// Link circuits per `(node, worker)` (production `LinkHealth`, worker-local).
    links: BTreeMap<(u32, u8), Links>,
    /// Running acquisitions by `(node, worker, read)`: when each started and how its walk
    /// probes ended, for origin repeat classification ([`repeats`]).
    acquisitions: BTreeMap<(u32, u32, ReadId), Walked>,
    /// Acquisitions recorded since the last prune of ended ones.
    acquisitions_added: usize,
    /// Why a walk probe's request failed in transport, by `(node, worker, effect)`.
    cuts: BTreeMap<(u32, u32, EffectId), Cut>,
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
    /// Last links awaiting their destination's answer: sender, destination, page, send time.
    last_hops: BTreeMap<u64, (u32, u32, Page, Nanos)>,
    /// Silence pings in flight by id: requester and destination.
    pings: BTreeMap<u64, (u32, u32)>,
    /// Partitioned node pairs, smaller id first.
    partitions: BTreeSet<(u32, u32)>,
    /// Route searches: the real `topology::Paths` over exhaustive mesh searches, one cache
    /// shared by every node. Production keeps one per worker (`src/app.rs` `Paths::with_limits`),
    /// but a cache entry's routes depend only on its key, not on the
    /// node that searched (`topology/src/paths.rs` `PathKey` leaves out the seed, which only
    /// picks among the cached alternatives), so sharing changes search counts and CPU, not
    /// which relays carry requests. Per-node caches of production's 128 entries left the 10,000
    /// node cold start unchanged (130 against 134 failures, 0 search sheds) at 18 KB more per
    /// node. Searches run synchronously, so the search queue never fills.
    paths: Paths<Exhaustive>,
    /// Peer connection slots of every node (`racer_core::connections`).
    conns: Connections,
    /// The connections each request holds, one per link in route order.
    msg_conns: BTreeMap<u64, Vec<ConnId>>,
    /// Requests whose next hop waits in an accept queue: the request and when it arrives.
    queued: BTreeMap<ConnId, (u64, Nanos)>,
    /// Requests whose answer is on its way back, so their connections go idle, not closed.
    answered_msgs: BTreeSet<u64>,
}

const TAG_EVENT: u64 = 1;
const TAG_READ_END: u64 = 2;
const TICK: Nanos = 50 * MILLIS;
/// Per-attempt exchange cap.
pub const ATTEMPT_TIMEOUT: Nanos = 2 * SECONDS;
/// No per-attempt cap: an attempt ends at its overall deadline.
pub const NO_ATTEMPT_CAP: Nanos = Nanos::MAX;
/// A serving read's own scope.
const SERVE_TIMEOUT: Nanos = 5 * SECONDS;
/// Client attempts per read.
const MAX_ATTEMPTS: u8 = 4;
/// A link to an unreachable peer fails after this.
const CONNECT_TIMEOUT: Nanos = 100 * MILLIS;
/// Route searches per worker at once (production `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER`
/// default, `src/worker.rs`).
const ACTIVE_SEARCHES: usize = 8;
/// Route searches per worker that may wait for a slot (production `topology::DEFAULT_SEARCH_QUEUE`,
/// `src/app.rs`).
const SEARCH_QUEUE: usize = topology::DEFAULT_SEARCH_QUEUE;

/// When a new route search starts given the end times of a worker's searches still running or
/// waiting (`ends`, all after `now`): at once with a free slot, else in arrival order when the
/// [`ACTIVE_SEARCHES`]th-latest of them ends, or `None` (`Overloaded`) with [`SEARCH_QUEUE`]
/// already waiting. Every search's end is known when it starts, so FIFO admission over
/// [`ACTIVE_SEARCHES`] slots starts the next one exactly then.
fn search_start(ends: &[Nanos], now: Nanos) -> Option<Nanos> {
    if ends.len() < ACTIVE_SEARCHES {
        return Some(now);
    }
    if ends.len() >= ACTIVE_SEARCHES + SEARCH_QUEUE {
        return None;
    }
    let mut sorted = ends.to_vec();
    sorted.sort_unstable();
    Some(sorted[ends.len() - ACTIVE_SEARCHES])
}
/// Entries of the shared route cache.
const ROUTE_CACHE_ENTRIES: usize = 1 << 16;

/// The owners of `page` under `version`, as node ids.
fn owners(placement: &Placement<Phantom>, version: &Version, page: Page) -> Vec<u32> {
    let mut ranked = placement.rank(&version.members, &page.key());
    ranked.truncate(REPLICAS);
    ranked.into_iter().map(|p| version.node(p)).collect()
}

/// The Zipf CDF over `objects` ranks with exponent `s`.
fn zipf_cdf(objects: u64, s: f64) -> Vec<f64> {
    let n = objects.max(1);
    let mut cdf = Vec::with_capacity(n as usize);
    let mut sum = 0.0;
    for k in 1..=n {
        sum += 1.0 / (k as f64).powf(s);
        cdf.push(sum);
    }
    for v in &mut cdf {
        *v /= sum;
    }
    cdf
}

/// Uniform in `[0, 1)` from 53 random bits.
fn unit(rng: &mut SplitMix64) -> f64 {
    (rng.next_u64() >> 11) as f64 * (1.0 / (1u64 << 53) as f64)
}

/// The worker of a node that owns `page`'s flight.
fn owner_worker(page: Page, workers: u32) -> u32 {
    (page.object.wrapping_mul(0x9e37_79b9_7f4a_7c15) ^ u64::from(page.number)) as u32 % workers
}
