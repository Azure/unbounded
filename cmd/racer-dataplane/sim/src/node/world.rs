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

use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::pin::Pin;
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
use racer_core::read::fanout::{fanout_helper, hot_page_bodies, is_helper, shed_hot_page};
use racer_core::read::fill::{FailureKind, FillPlan, FillStep, Source, WalkResult};
use racer_core::read::flight::{Event as FlightEvent, Joined};
use racer_core::read::policy::{
    Persist, PersistFacts, peer_error, persist, recoverable, retention_eligible, retention_score,
};
use racer_core::read::range::{PageBudget, SlotKind, direct_fills, first_failure};
use racer_core::read::serve::{ServeAction, ServeFacts, serve_action, serve_deadline};
use racer_core::read::stale::{StaleSkip, fallback_rank_allowed, previous_fallback};
use racer_core::read::successor::{MAX_DEFERRALS, defer_allowed, should_note, successor_deferral};
use racer_core::read::walk::{
    AttemptInputs, AttemptPlan, CandidateWalk, Continuation, End, HedgeFacts, Mode, Outcome,
    PREVIOUS_MEMBERSHIP_WITHIN, Probe, RemainingWalk, RouteMode, StaleAction, Step, WalkConfig,
    handoff_targets, hedge_budgets, hedge_eligible, hedge_fallback, plan_attempt,
    same_rank_previous, stale_action, transport_outcome,
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
use crate::engine::{Engine, MILLIS, Nanos, Role, SECONDS, Streams, mix};
use crate::faults::{Fault as SimFault, Partitions, origin_errors_at};
use crate::report::{Invariants, Metrics, peak_rss_bytes};
use crate::resources::{Fabric, Origin, OriginHandle, OriginOutcome};
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
    /// `Overloaded` signed by the destination itself, not by a relay
    /// (production: `VerifiedResponse::origin` is the destination). The
    /// walk reports it as `Outcome::Shed`.
    Shed,
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
    /// The waiter a fill serves reached its deadline while the fill's origin
    /// request was outstanding: hand the fill over to another live waiter,
    /// or cancel it like production's fill driver (`src/read/fill.rs`,
    /// `FlightOperation::poll_cancellation` and `owned_scope.cancel()`).
    OriginCancel {
        fill: u64,
    },
    /// Deliver moved origin completions that are due (`origin_moved`).
    OriginWake,
    /// A hedge race's duplicate delay elapsed.
    HedgeDelay {
        fill: u64,
    },
    /// A relayed reply finished streaming (or was abandoned): release the
    /// relays' `Relay` reservations held under `id`.
    Release {
        id: u64,
    },
    /// A ranking or route search task yielded; run its next slice.
    Slice {
        task: u64,
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
                    Reply::Shed => 1_000,
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
            Self::Slice { task } => (16, task, 0),
            Self::HedgeDelay { fill } => (17, fill, 0),
            Self::Release { id } => (18, id, 0),
            Self::OriginCancel { fill } => (19, fill, 0),
            Self::OriginWake => (20, 0, 0),
        }
    }
}

type TaskFuture = Pin<Box<dyn Future<Output = Result<Vec<usize>, topology::Error>>>>;

/// What to do with a finished ranking or route search.
enum Then {
    /// Placement ranking for a fill's candidate walk.
    Walk { fill: u64, version: u64 },
    /// An owner's ranking of the page under its previous membership, for
    /// copy-only handoff probes before origin.
    Handoff { fill: u64 },
    /// Ranking under membership `version` (the previous one) for the stale
    /// fallback (`NodeWorld::stale_fallback`).
    Fallback { fill: u64, version: u64 },
    /// Re-ranking before origin (production `resolve_epoch_traced`).
    Origin {
        fill: u64,
        rank: usize,
        evidence: Vec<ProbeOutcome>,
    },
    /// Route search for a message at `holder`.
    Route {
        msg: u64,
        holder: u32,
        plan: Option<RelayPlan>,
        blocked: Vec<usize>,
    },
}

/// A yielding ranking or route search: the real future, polled once per
/// simulated CPU slice on its worker's CPU FIFO (see the design doc,
/// "Round 2: fidelity").
struct Task {
    node: u32,
    worker: u8,
    incarnation: u32,
    future: TaskFuture,
    members: Rc<Members>,
    /// CPU per poll after the first (ranking: one chunk for every poll).
    slice_ns: Nanos,
    first_ns: Nanos,
    polls: u64,
    route: bool,
    then: Then,
    done: Option<Result<Vec<usize>, topology::Error>>,
    /// Pending without having woken itself: waiting in the route search
    /// queue (`Paths::with_queue`) until another task wakes it.
    parked: bool,
}

/// A task waker: records the task id so the world can schedule its next
/// slice. Searches wake themselves after each step; a queued search is woken
/// by the task that frees a slot.
struct TaskWaker {
    task: u64,
    woken: std::sync::Arc<std::sync::Mutex<Vec<u64>>>,
}

impl std::task::Wake for TaskWaker {
    fn wake(self: std::sync::Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &std::sync::Arc<Self>) {
        self.woken.lock().expect("single thread").push(self.task);
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
    /// Previous-membership owners to ask before origin (owners), or the
    /// page's overflow helper (non-owners); probe canonical `cands.len() + i`
    /// addresses `handoff[i]`.
    handoff: Vec<u32>,
    /// This owner is a member of membership version - 1, which is still
    /// retained and was replaced within `PREVIOUS_MEMBERSHIP_WITHIN`.
    previous_member: bool,
    /// Predecessors to ask under membership version - 1
    /// (`WalkConfig::previous`).
    previous: u8,
    version: u64,
    rank: Option<usize>,
    retried: bool,
    /// Owners this walk skipped as stale destinations (behind this node).
    stale: u32,
    /// This fill is ranking, or already walked, the previous membership
    /// after a destination under the current one was behind
    /// (`stale_fallback`).
    fell_back: bool,
    /// This node owns the page under the previous membership, so this walk
    /// keeps the current one.
    fallback_denied: bool,
    probe: Option<u64>,
    /// The leader's retention observation, used at publication.
    eligible: bool,
    order: VisitOrder,
    continuation: Continuation,
    hedge: Option<Box<Race>>,
    _reservation: Vec<SimCharge>,
    /// When the fill started (repeat classification).
    started: Nanos,
    /// Destination of the outstanding walk probe.
    asking: Option<u32>,
    /// Why the outstanding walk probe failed in transport, if it did.
    cut: Option<Cut>,
    /// Walk probes so far: destination and how each ended (repeat
    /// classification, [`NodeWorld::classify_repeat`]).
    asked: Vec<(u32, Asked)>,
    /// Outstanding origin request and its outcome (drawn at issue).
    origin: Option<(OriginHandle, OriginOutcome)>,
    /// The fill was canceled at its scope (Known issue 29).
    canceled: bool,
    /// Its origin request overlapped another node's request for the page
    /// (`origin_repeats_concurrent`, Known issue 31).
    concurrent: bool,
}

/// How one walk probe ended, for repeat classification.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Asked {
    Copy,
    Miss,
    /// Transport failure, and why (see [`Cut`]).
    Unreachable(Cut),
    /// The destination answered with a failure of its own (for a
    /// delegation: rank 0's own acquisition, such as its origin request,
    /// failed).
    Failed,
    /// The destination or a relay shed the request, or it was behind.
    Busy,
    /// The probe could not be planned (budget) or failed terminally.
    Unfunded,
}

/// Why a walk probe failed in transport (repeat classification).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Cut {
    /// A node on the way, or the destination, is a member that does not
    /// serve yet (no membership; Known issue 14).
    NotServing,
    /// A site partition cut a link on the way.
    Partition,
    /// A node on the way, or the destination, is down.
    Down,
    /// No route within the probe's links, or the route changed.
    Route,
    /// A link circuit was open (backoff after earlier failures).
    Circuit,
    /// The attempt timed out or a relay failed otherwise.
    Other,
}

/// Skip causes (`classify_repeat`), as `&'static` counter names.
const SKIPS: [&str; 10] = [
    "not_serving",
    "partition",
    "down",
    "route",
    "circuit",
    "timeout",
    "failed",
    "busy",
    "copy_only",
    "unfunded",
];

/// `prefix` + `skip` as a static counter name (`copy` never orphans).
fn skip_counter(prefix: &'static str, skip: &'static str) -> &'static str {
    match (prefix, skip) {
        ("origin_skips_", "not_serving") => "origin_skips_not_serving",
        ("origin_skips_", "partition") => "origin_skips_partition",
        ("origin_skips_", "down") => "origin_skips_down",
        ("origin_skips_", "route") => "origin_skips_route",
        ("origin_skips_", "circuit") => "origin_skips_circuit",
        ("origin_skips_", "timeout") => "origin_skips_timeout",
        ("origin_skips_", "failed") => "origin_skips_failed",
        ("origin_skips_", "busy") => "origin_skips_busy",
        ("origin_skips_", "copy_only") => "origin_skips_copy_only",
        ("origin_skips_", "copy") => "origin_skips_copy",
        ("origin_skips_", _) => "origin_skips_unfunded",
        (_, "not_serving") => "origin_repeats_orphan_not_serving",
        (_, "partition") => "origin_repeats_orphan_partition",
        (_, "down") => "origin_repeats_orphan_down",
        (_, "route") => "origin_repeats_orphan_route",
        (_, "circuit") => "origin_repeats_orphan_circuit",
        (_, "timeout") => "origin_repeats_orphan_timeout",
        (_, "failed") => "origin_repeats_orphan_failed",
        (_, "busy") => "origin_repeats_orphan_busy",
        (_, "copy_only") => "origin_repeats_orphan_copy_only",
        (_, "copy") => "origin_repeats_orphan_copy",
        (_, _) => "origin_repeats_orphan_unfunded",
    }
}

/// `origin_repeats_concurrent_skip_<skip>`: a concurrent repeat by an owner
/// above rank 0, by why it skipped rank 0 (Known issue 31).
fn concurrent_counter(skip: &'static str) -> &'static str {
    match skip {
        "not_serving" => "origin_repeats_concurrent_skip_not_serving",
        "partition" => "origin_repeats_concurrent_skip_partition",
        "down" => "origin_repeats_concurrent_skip_down",
        "route" => "origin_repeats_concurrent_skip_route",
        "circuit" => "origin_repeats_concurrent_skip_circuit",
        "timeout" => "origin_repeats_concurrent_skip_timeout",
        "failed" => "origin_repeats_concurrent_skip_failed",
        "busy" => "origin_repeats_concurrent_skip_busy",
        "copy_only" => "origin_repeats_concurrent_skip_copy_only",
        "copy" => "origin_repeats_concurrent_skip_copy",
        _ => "origin_repeats_concurrent_skip_unfunded",
    }
}

/// The last origin request for a page ([`NodeWorld::classify_repeat`]).
#[derive(Clone, Copy, Debug)]
struct LastOrigin {
    node: u32,
    /// The page's rank 0 under that request's membership.
    rank0: u32,
    /// `None` while in flight, then whether it succeeded.
    ok: Option<bool>,
    /// When it completed.
    done: Nanos,
    /// For a fetch by an owner above rank 0: why rank 0 did not serve it
    /// (`origin_skips_*`).
    skip: Option<&'static str>,
    /// Membership version of that request.
    version: u64,
}

impl Fill {
    /// Outstanding probe ids, including hedge contenders.
    fn probes(&self) -> impl Iterator<Item = u64> + use<> {
        let race = self.hedge.as_ref();
        [
            self.probe,
            race.and_then(|r| r.primary),
            race.and_then(|r| r.secondary),
        ]
        .into_iter()
        .flatten()
    }
}

struct Waiting {
    node: u32,
    incarnation: u32,
    worker: u8,
    page: u64,
    id: u64,
    want: Want,
    /// Retention observation latched when this operation joined.
    eligible: bool,
    /// Budget and plaintext need, kept so a waiter re-elected after the
    /// leader's origin rejection can lead its own fill.
    /// `None` once this waiter leads (its fill owns the budget).
    budget: Option<AcquisitionBudget>,
    plaintext: bool,
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
    /// When the requester abandons this exchange (its attempt timeout).
    abandon: Nanos,
    relays: Vec<SimCharge>,
    /// When the first relay took its `Relay` reservation.
    relay_first: Nanos,
    /// A pinned single-link hedge exchange.
    direct: bool,
}

struct Probing {
    fill: u64,
    acquire: bool,
    /// `Some(true)` for a hedge primary, `Some(false)` for its duplicate.
    hedge: Option<bool>,
}

/// One hedge race (production `hedge_page` and `drivers::race`).
struct Race {
    primary_budget: AcquisitionBudget,
    secondary_budget: AcquisitionBudget,
    /// Outstanding probe ids.
    primary: Option<u64>,
    secondary: Option<u64>,
    primary_done: bool,
    secondary_done: bool,
    /// The duplicate's delay elapsed (or the primary finished first).
    launched: bool,
    /// `Some(true)` when the primary won.
    winner: Option<bool>,
    fatal: Option<Error>,
    primary_outcome: Option<ProbeOutcome>,
    stale: bool,
    /// Budget before the split, for `budget_never_grows`.
    before: (u32, u8),
    first: usize,
    second: usize,
    _permit: flow_control::HedgePermit,
    _escrow: Vec<SimCharge>,
}

/// Production `DUPLICATE_BYTES` (`src/read/candidates.rs`).
pub const DUPLICATE_BYTES: usize = PAGE_BYTES * 2 + 16;

/// Production `reserve_hedge_pages`: plaintext and ciphertext for `pages`.
fn hedge_pages(
    quotas: &super::admission::SimQuotas,
    pages: usize,
) -> Result<Vec<SimCharge>, Error> {
    Ok(vec![
        reserve(quotas, ResourceClass::Plaintext, PAGE_BYTES * pages)?,
        reserve(quotas, ResourceClass::Ciphertext, (PAGE_BYTES + 16) * pages)?,
    ])
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
    /// Relay reservations of replies still streaming (see `Event::Release`).
    held: HashMap<u64, Vec<SimCharge>, Fixed>,
    /// End times of page bodies each (node, page) is still streaming
    /// (production: page responses being written, `send_http_response`).
    streaming: HashMap<(u32, u64), Vec<Nanos>, Fixed>,
    /// End times of all page bodies each node is still streaming.
    bodies: HashMap<u32, Vec<Nanos>, Fixed>,
    origin_per_page: HashMap<u64, u32, Fixed>,
    /// Earliest scheduled [`Event::OriginWake`], which delivers origin
    /// completions a cancellation moved earlier ([`Origin::pop_moved`]), so
    /// a cancellation schedules one event, not one per moved request.
    origin_wake: Option<Nanos>,
    /// Per page: the node that last asked origin for it and the page's rank 0
    /// under that request's membership (see [`Self::classify_repeat`]).
    last_origin: HashMap<u64, LastOrigin, Fixed>,
    tasks: HashMap<u64, Task, Fixed>,
    /// When each currently down node went down (Known issue 7 evidence).
    down_since: HashMap<u32, Nanos, Fixed>,
    /// Task ids woken since the last drain (see [`TaskWaker`]).
    woken: std::sync::Arc<std::sync::Mutex<Vec<u64>>>,
    publish_scheduled: bool,
    profile: Profile,
}

/// Host wall time spent in the real topology code, for the report only
/// (never feeds simulated time or decisions).
#[derive(Debug, Default)]
struct Profile {
    rank_ns: u64,
    route_ns: u64,
    publish_ns: u64,
}

#[allow(clippy::disallowed_methods)] // Profiling only; see `Profile`.
fn wall_clock() -> Instant {
    Instant::now()
}

impl NodeWorld {
    pub fn new(config: Config, seed: u64) -> Self {
        Self::build(config, seed, false)
    }

    /// Combined mode: membership versions come from the control plane
    /// ([`Self::commit_membership`]) and each node adopts the version its
    /// control client applied ([`Self::apply_membership`]). No node is
    /// enrolled until its client applies a version; until then it neither
    /// reads nor accepts peer links.
    pub fn new_combined(config: Config, seed: u64) -> Self {
        Self::build(config, seed, true)
    }

    fn build(config: Config, seed: u64, combined: bool) -> Self {
        let streams = Streams::new(seed);
        let fabric = Fabric::new(&config, &mut streams.stream(Role::Topology, 0));
        let origin = Origin::new(&config, streams.stream(Role::Origin, 0));
        let n = config.nodes;
        // Instants are only used as offsets from this base, so runs replay.
        let base = wall_clock();
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
            versions: if combined {
                Versions::external(n, node_config.retained_versions)
            } else {
                Versions::new(
                    n,
                    node_config.membership_lag_ns,
                    node_config.retained_versions,
                )
            },
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
            held: HashMap::default(),
            streaming: HashMap::default(),
            bodies: HashMap::default(),
            origin_per_page: HashMap::default(),
            origin_wake: None,
            last_origin: HashMap::default(),
            tasks: HashMap::default(),
            down_since: HashMap::default(),
            woken: std::sync::Arc::default(),
            publish_scheduled: false,
            profile: Profile::default(),
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
        while self.step(end) {}
        self.engine.finish(end);
    }

    /// Process the next event at or before `end`. Returns false when none.
    pub fn step(&mut self, end: Nanos) -> bool {
        let Some(event) = self.engine.pop_until(end) else {
            return false;
        };
        let now = self.engine.now();
        let (tag, a, b) = event.tag();
        self.engine.trace.record(now, tag, a, b);
        self.handle(event);
        true
    }

    /// Combined mode: the control plane committed membership `number` with
    /// `members` (node ids) at `now`. Prunes versions no live node needs.
    pub fn commit_membership(&mut self, number: u64, members: Vec<u32>, now: Nanos) {
        let started = wall_clock();
        let added = self.versions.commit(number, members, now);
        self.profile.publish_ns += started.elapsed().as_nanos() as u64;
        if !added {
            return;
        }
        self.metrics.incr("membership_publications");
        self.engine.trace.record(now, 15, number, 0);
        self.prune_versions();
    }

    /// Combined mode: `node`'s control client applied `number` (0 when the
    /// client restarted and has no snapshot yet).
    pub fn apply_membership(&mut self, node: u32, number: u64) {
        if self.valid(node) {
            self.versions.apply(node, number);
        }
    }

    /// Combined mode: drop versions no live node can resolve.
    pub fn prune_versions(&mut self) {
        let nodes = &self.nodes;
        if self.versions.prune(|n| nodes[n as usize].down) {
            self.shared.phantom.forget(&self.versions.identities());
            self.shared
                .routes
                .forget(&self.versions.topology_identities());
        }
    }

    /// Whether `node` is crashed or removed.
    pub fn is_down(&self, node: u32) -> bool {
        self.nodes.get(node as usize).is_none_or(|n| n.down)
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
                    match p.hedge {
                        Some(primary) => {
                            self.hedge_result(p.fill, primary, Err(Error::Unavailable))
                        }
                        None => self.complete_probe(
                            p.fill,
                            Ok(Outcome::Transport(ProbeOutcome::Unreachable)),
                        ),
                    }
                }
            }
            Event::OriginDone { fill, .. } => self.origin_done(fill),
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
                    self.down_since.remove(&node);
                    self.fabric.reset_node(node, self.now());
                    self.metrics.incr("restarts");
                }
            }
            Event::Slow { node, factor } => {
                if self.valid(node) {
                    self.fabric.slow[node as usize] = factor.max(1);
                }
            }
            Event::Slice { task } => self.slice(task),
            Event::HedgeDelay { fill } => self.hedge_delay(fill),
            Event::OriginCancel { fill } => self.origin_cancel(fill),
            Event::OriginWake => self.origin_wake(),
            Event::Release { id } => {
                self.held.remove(&id);
            }
        }
    }

    fn publish(&mut self) {
        self.publish_scheduled = false;
        let now = self.now();
        let started = wall_clock();
        let dropped = self.versions.publish(now);
        self.profile.publish_ns += started.elapsed().as_nanos() as u64;
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
            if let Some(fill) = self.fills.remove(&id) {
                // A crash closes the node's origin connections.
                if let Some((handle, ..)) = fill.origin {
                    self.cancel_origin(handle);
                }
                for probe in fill.probes() {
                    self.probes.remove(&probe);
                }
            }
        }
        self.tokens.retain(|_, w| w.node != node);
        // Dropping a task's future releases its placement pin or search slot.
        self.tasks.retain(|_, t| t.node != node);
        self.wake_tasks(None);
        self.ranges.retain(|_, r| r.node != node);
        let now = self.instant(self.now());
        let node_config = self.config.node;
        let n = &mut self.nodes[node as usize];
        n.restart(&node_config, &self.shared, self.config.disk_pages, now);
        n.down = true;
        self.down_since.insert(node, self.now());
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
        if !self.versions.enrolled(node) {
            self.metrics.incr("reads_skipped_not_enrolled");
            return;
        }
        // Not listed in the membership it holds: production `/readyz` is
        // false (`WorkerApplication::membership_usable`) and a read could
        // not route. A new node is listed only after it serves (Known
        // issue 14), a little after it applied its first membership.
        if position(&self.versions.adopted(node, now).members, node).is_none() {
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
        if !self.versions.enrolled(node) {
            self.metrics.incr("reads_skipped_not_enrolled");
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
        // Production observes retention once per client operation, before
        // the memory lookup result is used (`src/read/fill.rs`
        // `acquire_with_prefetch`: `retention.observe` on a plaintext hit and
        // before joining a flight); peer acquires use `owned_only`.
        let eligible = if peer {
            self.owned(node, w, page)
        } else {
            self.observe(node, w, page)
        };
        let n = &mut self.nodes[node as usize];
        let worker = &mut n.workers[w as usize];
        if worker.memory.pages.contains_key(&page) {
            let _ = worker.memory.touch(&page);
            self.metrics.incr(if peer {
                "peer_memory_hits"
            } else {
                "memory_hits"
            });
            if !peer {
                self.persist_page(node, w, page, eligible);
            }
            self.resolve_at(want, Ok(Got::Local), t);
            return;
        }
        if n.disk.entries.get(&page).is_some() {
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
                if !peer {
                    self.persist_page(node, w, page, eligible);
                }
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
                        eligible,
                        budget: None,
                        plaintext,
                    },
                );
                match event {
                    Ok(FlightEvent::Lead) => {
                        self.start_fill(node, w, page, id, token, budget, plaintext, eligible);
                    }
                    Ok(FlightEvent::Pending) => {
                        if let Some(waiting) = self.tokens.get_mut(&token) {
                            waiting.budget = Some(budget);
                        }
                        self.metrics.incr("flight_joins");
                    }
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
            Ok(FlightEvent::Complete(_)) => {
                if let Want::Client(_) = waiting.want {
                    self.persist_page(waiting.node, waiting.worker, waiting.page, waiting.eligible);
                }
                Ok(Got::Joined)
            }
            Ok(FlightEvent::Failed(e)) | Err(e) => Err(e),
            Ok(FlightEvent::Lead) if waiting.budget.is_some() => {
                // The previous leader's origin attempt was rejected for its
                // credentials (`PageFlights::reject`); this waiter fills with
                // its own budget, as production re-elects in `Flights::fail`.
                self.metrics.incr("flight_reelected");
                let mut waiting = waiting;
                let budget = waiting.budget.take().expect("checked");
                let (node, w, page, id) = (waiting.node, waiting.worker, waiting.page, waiting.id);
                let (plaintext, eligible) = (waiting.plaintext, waiting.eligible);
                self.tokens.insert(token, waiting);
                self.start_fill(node, w, page, id, token, budget, plaintext, eligible);
                for token in wakes {
                    self.wake(token);
                }
                return;
            }
            Ok(FlightEvent::Lead) => {
                // A waiter whose budget already moved to a fill cannot lead
                // twice.
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
                            // When reads fail, to attribute them to faults.
                            self.metrics
                                .observe("read_failed_at_s", self.now() / SECONDS);
                            if !self.down_since.is_empty() {
                                self.metrics.incr("reads_failed_while_crashed");
                            }
                            if self.versions.unready_members() > 0 {
                                // Known issue 17: reads failing while some
                                // member cannot serve yet.
                                self.metrics.incr("reads_failed_while_members_unready");
                            }
                            self.range_slot_done(r, false);
                        }
                    }
                }
            }
            Want::Peer { msg } => {
                let Some(m) = self.msgs.get(&msg) else {
                    return;
                };
                let (dest, page) = (m.dest, m.page);
                match result {
                    Ok(_) => self.send_reply(msg, dest, Reply::Response(ResponseKind::Page), now),
                    Err(e) => {
                        let requester = m.requester;
                        self.note_hint(dest, page, requester, false);
                        let reply = peer_error(e).map_or(Reply::Transport, Reply::Response);
                        self.send_reply(msg, dest, reply, now);
                    }
                }
            }
        }
    }

    /// Production `Coordinator::serve_peer` (Known issue 21): `node` holds
    /// no copy of `page` and a hint naming another current owner that
    /// fetched it after `node` answered it without a copy; a non-owner
    /// requester is answered `Miss` and asks that owner next.
    fn defer_to_successor(&mut self, node: u32, page: u64, requester: u32) -> bool {
        if !self.config.node.successor_hints {
            return false;
        }
        let w = self.ids.worker(page);
        let instant = self.instant(self.now());
        let n = &self.nodes[node as usize];
        if n.hints.is_empty()
            || n.workers[w as usize].memory.pages.contains_key(&page)
            || n.disk.entries.get(&page).is_some()
        {
            return false;
        }
        let hints = n.hints.peek(&page, instant);
        if hints.is_empty() {
            return false;
        }
        let acquiring = n.workers[w as usize]
            .flights
            .acquiring(&self.ids.page(page));
        if !defer_allowed(acquiring) {
            return false;
        }
        let owners = self.owners_at(node, w, page, true);
        if successor_deferral(hints, &node, &requester, &owners).is_none() {
            return false;
        }
        self.nodes[node as usize].hints.charge(&page, MAX_DEFERRALS);
        true
    }

    /// The page's owners under the membership `at` adopted, ranking them
    /// when `rank` (production `CandidatePolicy::candidates_scoped`) or only
    /// from the placement cache (production `Placement::cached`).
    fn owners_at(&self, at: u32, w: u8, page: u64, rank: bool) -> Vec<u32> {
        let n = &self.nodes[at as usize];
        let Some(version) = self.versions.get(n.version) else {
            return Vec::new();
        };
        let key = page.to_be_bytes();
        let placement = &n.workers[w as usize].placement;
        let ranked = if rank {
            placement.rank(&*version.members, &key)
        } else {
            placement
                .cached(&*version.members, &key)
                .unwrap_or_default()
        };
        ranked
            .into_iter()
            .take(3)
            .map(|p| version.members.members()[p].node)
            .collect()
    }

    /// `node` answered `requester`'s page probe without a copy (production
    /// `Coordinator::serve_peer`, Known issue 21). A copy-only miss is noted
    /// without ranking (`successor_deferral` checks ownership); a failed
    /// acquisition checks the ranking its own walk cached, so neither path
    /// ranks a page or disturbs the placement cache.
    fn note_hint(&mut self, node: u32, page: u64, requester: u32, copy_only: bool) {
        if !self.config.node.successor_hints || node == requester {
            return;
        }
        let w = self.ids.worker(page);
        if !copy_only && !should_note(&node, &requester, &self.owners_at(node, w, page, false)) {
            return;
        }
        self.metrics.incr("successor_hints_noted");
        let instant = self.instant(self.now());
        self.nodes[node as usize]
            .hints
            .note(page, requester, instant);
    }

    pub(super) fn memory_insert(&mut self, node: u32, w: u8, page: u64) {
        let cap = (self.config.memory_pages / u32::from(self.fabric.workers)).max(1) as usize;
        let memory = &mut self.nodes[node as usize].workers[w as usize].memory;
        // Production `MemoryCache::publish` keeps a resident page's position:
        // a duplicate publish is not a use (`src/memory.rs`, "must not ...
        // turn its historical deadline into renewed freshness").
        if memory.pages.contains_key(&page) {
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
        eligible: bool,
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
        // Production retains the fill as a flight operation before submitting
        // it (`src/read/fill.rs`, `retain_operation`): a revoked leader's
        // still-running fill keeps the flight draining, so no second leader
        // starts a concurrent fill for the page (Known issue 24).
        let retained = self.nodes[node as usize].workers[w as usize]
            .flights
            .retain_operation(&self.ids.page(page), leader);
        self.invariants.check(
            "flight_leader_valid",
            retained.is_ok(),
            self.engine.now(),
            Some(node),
            || format!("retain page {page} leader {leader}: {retained:?}"),
        );
        // Production lets the running fill pass to another live waiter when
        // its leader expires (Known issue 30, `Flights::allow_handover`),
        // bounded by a hard ceiling on its owned scope.
        let handover = self.config.node.flight_handover;
        if handover {
            let _ = self.nodes[node as usize].workers[w as usize]
                .flights
                .allow_handover(&self.ids.page(page), leader);
        }
        let scope = racer_core::read::flight::handover_ceiling(
            self.instant(self.engine.now()),
            budget.deadline(),
            handover,
        );
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
                handoff: Vec::new(),
                previous_member: false,
                previous: 0,
                version: 0,
                rank: None,
                retried: false,
                stale: 0,
                fell_back: false,
                fallback_denied: false,
                probe: None,
                eligible,
                order: VisitOrder::canonical(0),
                continuation: Continuation::default(),
                hedge: None,
                _reservation: reservation,
                started: self.engine.now(),
                asking: None,
                cut: None,
                asked: Vec::new(),
                origin: None,
                canceled: false,
                concurrent: false,
            },
        );
        self.metrics.incr("fills");
        match failed {
            Some(e) => self.finish_fill(id, Err(e)),
            None => self.begin_walk(id),
        }
    }

    /// Start a yielding task and run its first slice now.
    #[allow(clippy::too_many_arguments)]
    fn spawn(
        &mut self,
        node: u32,
        worker: u8,
        future: TaskFuture,
        members: Rc<Members>,
        route: bool,
        then: Then,
    ) {
        let costs = self.fabric.costs;
        let (first_ns, slice_ns) = if route {
            // A cache hit answers on the first poll; each further poll
            // expands up to 32 nodes (`SEARCH_QUANTUM`).
            (0, 32 * costs.path_expand_ns_per_node)
        } else {
            // Each poll scores up to 256 members (`WORK_QUANTUM`).
            (costs.ranking_chunk_ns, costs.ranking_chunk_ns)
        };
        let id = self.fresh();
        self.tasks.insert(
            id,
            Task {
                node,
                worker,
                incarnation: self.nodes[node as usize].incarnation,
                future,
                members,
                slice_ns,
                first_ns,
                polls: 0,
                route,
                then,
                done: None,
                parked: false,
            },
        );
        self.slice(id);
    }

    /// Run one slice of `task`: deliver a finished result, or poll the
    /// future once and reserve that poll's CPU on the worker FIFO. Other
    /// work queued on the CPU meanwhile runs before the next slice, as an
    /// executor runs other tasks after a yield.
    fn slice(&mut self, id: u64) {
        let now = self.now();
        let Some(task) = self.tasks.get_mut(&id) else {
            return;
        };
        if task.done.is_some() {
            let task = self.tasks.remove(&id).expect("present");
            return self.task_done(task);
        }
        task.parked = false;
        let started = wall_clock();
        let waker = Waker::from(std::sync::Arc::new(TaskWaker {
            task: id,
            woken: self.woken.clone(),
        }));
        let mut cx = Context::from_waker(&waker);
        let poll = task.future.as_mut().poll(&mut cx);
        let spent = started.elapsed().as_nanos() as u64;
        task.polls += 1;
        let cost = if task.polls == 1 {
            task.first_ns
        } else {
            task.slice_ns
        };
        let (node, worker, route) = (task.node, task.worker, task.route);
        if route {
            self.profile.route_ns += spent;
            if task.polls > 1 {
                self.metrics.incr("route_search_polls");
            }
        } else {
            self.profile.rank_ns += spent;
            self.metrics.incr("ranking_polls");
        }
        let end = if cost == 0 {
            now
        } else {
            self.fabric.cpu(now, node, worker, cost)
        };
        let task = self.tasks.get_mut(&id).expect("present");
        if let Poll::Ready(result) = poll {
            if end <= now {
                let task = self.tasks.remove(&id).expect("present");
                self.wake_tasks(None);
                return self.task_done(Task {
                    done: Some(result),
                    ..task
                });
            }
            task.done = Some(result);
        } else if !self.wake_tasks(Some(id)) {
            // Queued for a search slot: no next slice until a wake.
            self.metrics.incr("route_search_queued");
            self.tasks.get_mut(&id).expect("present").parked = true;
            return;
        }
        self.wake_tasks(None);
        self.engine.at(end.max(now + 1), Event::Slice { task: id });
    }

    /// Schedule a slice now for every parked task woken since the last call.
    /// Returns whether `current` (the task being polled) woke itself.
    fn wake_tasks(&mut self, current: Option<u64>) -> bool {
        let woken = std::mem::take(&mut *self.woken.lock().expect("single thread"));
        let mut itself = false;
        let now = self.now();
        for id in woken {
            if Some(id) == current {
                itself = true;
                continue;
            }
            if let Some(task) = self.tasks.get_mut(&id)
                && task.parked
            {
                task.parked = false;
                self.metrics.incr("route_search_queue_wakes");
                self.engine.at(now, Event::Slice { task: id });
            }
        }
        itself
    }

    fn task_done(&mut self, task: Task) {
        let Task {
            node,
            incarnation,
            members,
            then,
            done,
            ..
        } = task;
        if self.nodes[node as usize].incarnation != incarnation {
            return;
        }
        let result = done.expect("finished task");
        if matches!(result, Err(topology::Error::Overloaded)) {
            self.metrics.incr(if matches!(then, Then::Route { .. }) {
                "route_search_overloaded"
            } else {
                "ranking_overloaded"
            });
        }
        match then {
            Then::Walk { fill, version } => {
                let cands = Self::to_nodes(&members, result);
                self.walk_ranked(fill, version, members, cands);
            }
            Then::Handoff { fill } => {
                let previous = Self::to_nodes(&members, result);
                self.handoff_ranked(fill, previous);
            }
            Then::Fallback { fill, version } => {
                let cands = Self::to_nodes(&members, result);
                self.fallback_ranked(fill, version, members, cands);
            }
            Then::Origin {
                fill,
                rank,
                evidence,
            } => {
                let expected = Self::to_nodes(&members, result);
                self.origin_ranked(fill, rank, evidence, members, expected);
            }
            Then::Route {
                msg,
                holder,
                plan,
                blocked,
            } => self.routed(msg, holder, plan, blocked, members, result),
        }
    }

    fn to_nodes(
        members: &Members,
        result: Result<Vec<usize>, topology::Error>,
    ) -> Result<Vec<u32>, Error> {
        let owners = result.map_err(|_| Error::Overloaded)?;
        Ok(owners
            .into_iter()
            .map(|p| members.members()[p].node)
            .collect())
    }

    /// Rank `page` with the worker's phantom placement as a yielding task:
    /// one CPU ranking chunk per poll, the cache entry pinned until done.
    fn rank(&mut self, node: u32, w: u8, members: Rc<Members>, page: u64, then: Then) {
        let placement = self.nodes[node as usize].workers[w as usize]
            .placement
            .clone();
        let held = members.clone();
        let future: TaskFuture = Box::pin(async move {
            let key = page.to_be_bytes();
            placement.rank_async(&*held, &key).await
        });
        self.spawn(node, w, future, members, false, then);
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
        self.rank(node, w, members, page, Then::Walk { fill: id, version });
    }

    fn walk_ranked(
        &mut self,
        id: u64,
        version: u64,
        members: Rc<Members>,
        cands: Result<Vec<u32>, Error>,
    ) {
        let Some(fill) = self.fills.get(&id) else {
            return;
        };
        let (node, page) = (fill.node, fill.page);
        let cands = match cands {
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
        fill.cands = cands;
        fill.stale = 0;
        fill.handoff.clear();
        fill.previous_member = false;
        fill.previous = 0;
        fill.version = version;
        fill.rank = rank;
        fill.order = order;
        // An owner also ranks the page under membership version - 1, when
        // still retained (production `CandidatePolicy::handoff_targets`).
        if rank.is_some()
            && let Some(old) = version.checked_sub(1).and_then(|v| self.versions.get(v))
        {
            let old = old.members.clone();
            // Production asks predecessors under the previous membership only
            // within `PREVIOUS_MEMBERSHIP_WITHIN` of replacing it, and only
            // with grace retention on (`Published::resolve_recent`), so peers
            // still resolve it. The simulator measures from the version's
            // publication, which is no later than this node's adoption, so
            // it is the stricter of the two; peers resolve older versions
            // only with `retained_versions` of at least 1.
            let recent = self.config.node.retained_versions >= 1
                && self.versions.get(version).is_some_and(|v| {
                    self.now().saturating_sub(v.published)
                        <= PREVIOUS_MEMBERSHIP_WITHIN.as_nanos() as Nanos
                });
            let fill = self.fills.get_mut(&id).expect("present");
            fill.previous_member = recent && position(&old, node).is_some();
            let w = fill.worker;
            self.rank(node, w, old, page, Then::Handoff { fill: id });
            return;
        }
        self.walk_owners(id);
    }

    /// Previous-membership owners are ranked: keep those that are still
    /// members and that the walk does not ask anyway, then walk.
    fn handoff_ranked(&mut self, id: u64, previous: Result<Vec<u32>, Error>) {
        let Some(fill) = self.fills.get(&id) else {
            return;
        };
        if let (Ok(previous), Some(rank)) = (previous, fill.rank) {
            let current = self.versions.get(fill.version).map(|v| v.members.clone());
            let targets = handoff_targets(&previous, &fill.cands, rank);
            let same = same_rank_previous(&previous, &fill.cands, rank);
            let fill = self.fills.get_mut(&id).expect("present");
            if fill.previous_member {
                fill.previous = same;
            }
            fill.handoff = targets
                .into_iter()
                .filter(|n| current.as_ref().is_some_and(|m| position(m, *n).is_some()))
                .collect();
        }
        self.walk_owners(id);
    }

    fn walk_owners(&mut self, id: u64) {
        let fill = self.fills.get_mut(&id).expect("present");
        if *fill.plan.step() == FillStep::Hedge && self.try_hedge(id) {
            return;
        }
        self.start_candidates(id);
    }

    /// Build the candidate walk from the fill's ranking and the carried
    /// continuation, then step it.
    fn start_candidates(&mut self, id: u64) {
        let fill = &self.fills[&id];
        let helpers_possible = self
            .versions
            .get(fill.version)
            .is_some_and(|v| v.members.members().len() > fill.cands.len() + 1);
        let fill = self.fills.get_mut(&id).expect("present");
        let continuation = std::mem::take(&mut fill.continuation);
        fill.walk = Walking::Candidates(CandidateWalk::new(WalkConfig {
            rank: fill.rank,
            owners: fill.cands.len(),
            order: fill.order,
            page_operation: true,
            continuation,
            handoff: fill.handoff.len(),
            previous: fill.previous,
            // A non-owner may ask the page's overflow helper when an owner
            // sheds it (production `resolve_epoch_traced`, Known issue 16);
            // the helper is chosen when the walk asks for it.
            helper: fill.rank.is_none() && self.config.node.fanout && helpers_possible,
        }));
        self.step_walk(id);
    }

    /// Whether `node` may open a pinned direct exchange to `dest` now
    /// (production `direct_hedge_available`: link health allows it).
    fn direct_available(&self, node: u32, dest: u32) -> bool {
        let instant = self.instant(self.now());
        self.nodes[node as usize].links.available(&dest, instant)
    }

    /// Production `hedge_page` (`src/read/candidates.rs`): start a hedge
    /// race when enabled, eligible, directly reachable, and admitted.
    /// Otherwise report no hedge to the plan and return false.
    fn try_hedge(&mut self, id: u64) -> bool {
        let now = self.now();
        let instant = self.instant(now);
        let fill = self.fills.get(&id).expect("present");
        let (node, w) = (fill.node, fill.worker);
        let enabled = self.config.node.hedge_slots > 0;
        let (first, second) = (
            fill.order.get(0).unwrap_or(0),
            fill.order.get(1).unwrap_or(0),
        );
        let facts = HedgeFacts {
            page_operation: true,
            is_candidate: fill.rank.is_some(),
            owners: fill.cands.len(),
            first,
            second,
            distinct: fill.cands.len() >= 2 && fill.cands[first] != fill.cands[second],
            remaining_attempts: fill.budget.remaining_attempts(),
            remaining_links: fill.budget.remaining_links(),
        };
        let eligible = hedge_eligible(facts);
        if eligible {
            self.metrics.incr("hedge_eligible");
        }
        if !enabled {
            self.fills.get_mut(&id).expect("present").plan.hedge(false);
            return false;
        }
        let (a, b) = (fill.cands[first], fill.cands[second]);
        let started = eligible && self.direct_available(node, a) && self.direct_available(node, b);
        let reservation = started
            .then(|| {
                let n = &self.nodes[node as usize];
                let quotas = &n.workers[w as usize].quotas;
                let escrow = hedge_pages(quotas, 1)?;
                // Fund both contenders before spending credits.
                drop(hedge_pages(quotas, 2)?);
                let delay = Duration::from_nanos(self.config.node.hedge_delay_ns);
                let permit = n
                    .hedges
                    .acquire(DUPLICATE_BYTES, instant + delay)
                    .map_err(|_| Error::Overloaded)?;
                Ok::<_, Error>((permit, escrow))
            })
            .and_then(Result::ok);
        let Some((permit, escrow)) = reservation else {
            if eligible {
                self.metrics.incr("hedge_suppressed");
            }
            self.fills.get_mut(&id).expect("present").plan.hedge(false);
            return false;
        };
        let fill = self.fills.get_mut(&id).expect("present");
        let (before_attempts, before_links) = (
            fill.budget.remaining_attempts(),
            fill.budget.remaining_links(),
        );
        let Ok((secondary, primary)) = hedge_budgets(&mut fill.budget) else {
            self.metrics.incr("hedge_suppressed");
            fill.plan.hedge(false);
            return false;
        };
        fill.continuation = Continuation {
            primary_consumed: true,
            primary_outcome: None,
            bounded_routes: true,
        };
        fill.hedge = Some(Box::new(Race {
            primary_budget: primary,
            secondary_budget: secondary,
            primary: None,
            secondary: None,
            primary_done: false,
            secondary_done: false,
            launched: false,
            winner: None,
            fatal: None,
            primary_outcome: None,
            stale: false,
            before: (before_attempts, before_links),
            first,
            second,
            _permit: permit,
            _escrow: escrow,
        }));
        self.metrics.incr("hedge_races");
        match self.send_hedge(id, true) {
            Ok(()) => {
                let delay = self.config.node.hedge_delay_ns;
                self.engine
                    .after(delay.max(1), Event::HedgeDelay { fill: id });
            }
            Err(e) => self.hedge_result(id, true, Err(e)),
        }
        true
    }

    /// Plan and send one hedge contender as a pinned direct exchange.
    fn send_hedge(&mut self, id: u64, primary: bool) -> Result<(), Error> {
        let now = self.now();
        let instant = self.instant(now);
        let attempt_timeout = Duration::from_nanos(self.config.node.attempt_timeout_ns);
        let fill = self.fills.get_mut(&id).expect("present");
        let scope = fill.scope.min(fill.budget.deadline());
        let race = fill.hedge.as_mut().expect("racing");
        let (budget, canonical, mode) = if primary {
            (&mut race.primary_budget, race.first, Mode::Acquire)
        } else {
            (&mut race.secondary_budget, race.second, Mode::CopyOnly)
        };
        let (before_attempts, before_links) =
            (budget.remaining_attempts(), budget.remaining_links());
        let plan = plan_attempt(
            budget,
            AttemptInputs {
                now: instant,
                caller_deadline: scope,
                remaining_opportunities: 3,
                route: RouteMode::DirectHedge,
                mode,
                attempt_timeout,
                failure_links: MAX_LINKS,
            },
        )?;
        let node = fill.node;
        self.invariants.check(
            "budget_never_grows",
            plan.links <= before_links && plan.links == 1 && plan.remote_attempts < before_attempts.max(1),
            now,
            Some(node),
            || format!("hedge plan {} links {} attempts from {before_links} links {before_attempts} attempts", plan.links, plan.remote_attempts),
        );
        let dest = fill.cands[canonical];
        let version = self.fills[&id].version;
        let pid = self.launch(
            id,
            dest,
            version,
            mode == Mode::Acquire,
            &plan,
            true,
            Some(primary),
        );
        let race = self
            .fills
            .get_mut(&id)
            .expect("present")
            .hedge
            .as_mut()
            .expect("racing");
        if primary {
            race.primary = Some(pid);
        } else {
            race.secondary = Some(pid);
        }
        Ok(())
    }

    /// The hedge delay elapsed: launch the copy-only duplicate unless the
    /// primary already finished or the race is decided.
    fn hedge_delay(&mut self, id: u64) {
        let Some(fill) = self.fills.get(&id) else {
            return;
        };
        let Some(race) = &fill.hedge else {
            return;
        };
        if race.launched || race.secondary_done || race.winner.is_some() || race.fatal.is_some() {
            return;
        }
        let (node, w, dest) = (fill.node, fill.worker, fill.cands[race.second]);
        // Recheck the link and local headroom after the delay.
        let quotas = &self.nodes[node as usize].workers[w as usize].quotas;
        let ok = self.direct_available(node, dest) && hedge_pages(quotas, 1).is_ok();
        let race = self
            .fills
            .get_mut(&id)
            .expect("present")
            .hedge
            .as_mut()
            .expect("racing");
        race.launched = true;
        if !ok {
            self.metrics.incr("hedge_suppressed");
            return self.hedge_result(id, false, Err(Error::Overloaded));
        }
        self.metrics.incr("hedge_started");
        self.metrics
            .add("hedge_duplicate_bytes", DUPLICATE_BYTES as u64);
        if let Err(e) = self.send_hedge(id, false) {
            self.hedge_result(id, false, Err(e));
        }
    }

    /// One hedge contender finished (production `record_result` in
    /// `uring_runtime::drivers::race`).
    fn hedge_result(&mut self, id: u64, primary: bool, result: Result<(), Error>) {
        let Some(fill) = self.fills.get_mut(&id) else {
            return;
        };
        let Some(race) = fill.hedge.as_mut() else {
            return;
        };
        if primary {
            race.primary = None;
            race.primary_done = true;
            if !race.launched {
                // A ready primary never launches the duplicate.
                race.secondary_done = true;
            }
        } else {
            race.secondary = None;
            race.secondary_done = true;
        }
        match result {
            Ok(()) if race.winner.is_none() && race.fatal.is_none() => {
                race.winner = Some(primary);
            }
            Err(e) if !recoverable(e) && race.winner.is_none() && race.fatal.is_none() => {
                race.fatal = Some(e);
            }
            _ => {}
        }
        let decided = race.winner.is_some() || race.fatal.is_some();
        let mut cancel = Vec::new();
        if decided {
            if let Some(pid) = race.primary.take() {
                race.primary_done = true;
                cancel.push(pid);
            }
            if let Some(pid) = race.secondary.take() {
                race.secondary_done = true;
                cancel.push(pid);
            }
            race.secondary_done |= !race.launched;
        }
        let finished = race.primary_done && race.secondary_done;
        for pid in cancel {
            // The loser is canceled: its exchange and relay charges are
            // released now. Production waits for the loser's completion
            // fence; the simulator releases at cancellation.
            self.metrics.incr("hedge_losers_canceled");
            self.probes.remove(&pid);
            if let Some(mid) = self
                .msgs
                .iter()
                .find(|(_, m)| m.probe == pid)
                .map(|(k, _)| *k)
            {
                self.msgs.remove(&mid);
            }
        }
        if finished {
            self.finish_race(id);
        }
    }

    /// Both contenders are fenced: reunite budgets and either publish the
    /// winner or fall back to the serial walk.
    fn finish_race(&mut self, id: u64) {
        let now = self.now();
        let fill = self.fills.get_mut(&id).expect("present");
        let race = *fill.hedge.take().expect("racing");
        let node = fill.node;
        let reunited = fill
            .budget
            .reunite(race.primary_budget)
            .and_then(|()| fill.budget.reunite(race.secondary_budget));
        let (attempts, links) = (
            fill.budget.remaining_attempts(),
            fill.budget.remaining_links(),
        );
        let (before_attempts, before_links) = race.before;
        self.invariants.check(
            "budget_never_grows",
            reunited.is_ok() && attempts <= before_attempts && links <= before_links,
            now,
            Some(node),
            || format!("hedge reunited {links} links {attempts} attempts from {before_links} links {before_attempts} attempts"),
        );
        let fill = self.fills.get_mut(&id).expect("present");
        fill.continuation.primary_outcome =
            Some(race.primary_outcome.unwrap_or(ProbeOutcome::Unreachable));
        if let Err(e) = reunited {
            return self.finish_fill(id, Err(e));
        }
        if let Some(primary) = race.winner {
            self.metrics.incr(if primary {
                "hedge_primary_won"
            } else {
                "hedge_won"
            });
            fill.continuation = Continuation::default();
            fill.plan.hedge(true);
            return self.continue_plan(id);
        }
        if let Some(e) = race.fatal
            && !hedge_fallback(e)
        {
            return self.finish_fill(id, Err(e));
        }
        fill.plan.hedge(false);
        self.metrics.incr("hedge_fallbacks");
        if race.stale {
            // Production `resolve_epoch_traced`: a stale hedge refreshes the
            // candidates and walks the new epoch with bounded routes.
            fill.continuation = Continuation {
                bounded_routes: true,
                ..Continuation::default()
            };
            let (retried, previous) = (fill.retried, fill.version);
            let latest = self.adopt(node);
            return match stale_action(retried, previous, Some(latest)) {
                Ok(StaleAction::Refresh) => {
                    self.metrics.incr("walk_membership_refreshes");
                    self.fills.get_mut(&id).expect("present").retried = true;
                    self.begin_walk(id)
                }
                // The stale contender is behind this node (Known issue 3):
                // walk this epoch with bounded routes.
                Ok(StaleAction::Skip) => {
                    self.metrics.incr("stale_destination_skips");
                    self.start_candidates(id)
                }
                Err(e) => self.fail_walk(id, e),
            };
        }
        self.start_candidates(id);
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
        let fill = &self.fills[&id];
        let dest = match probe.canonical.checked_sub(fill.cands.len()) {
            Some(i) if fill.rank.is_some() => {
                self.metrics.incr("handoff_probes");
                fill.handoff[i]
            }
            Some(_) => match self.fanout_helper(id) {
                Some(helper) => {
                    self.metrics.incr("helper_probes");
                    helper
                }
                // Every candidate helper is excluded: a miss.
                None => return Err(Some(ProbeOutcome::CopyMiss)),
            },
            None => fill.cands[probe.canonical],
        };
        let fill = self.fills.get_mut(&id).expect("present");
        // Peer exchanges stay within the leader's own budget deadline
        // (`src/read/candidates.rs`, `scope.deadline.0.min(budget.deadline())`).
        let scope = fill.scope.min(fill.budget.deadline());
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
                fill.asked.push((dest, Asked::Unfunded));
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
        let version = fill.version - u64::from(probe.previous);
        // Predecessors that held the same rank under the previous membership
        // are asked under it (`Probe::previous`, Known issue 12).
        if probe.previous {
            self.metrics.incr("previous_membership_probes");
        }
        // Set before launching: the first hop may fail synchronously.
        fill.asking = Some(dest);
        fill.cut = None;
        let pid = self.launch(id, dest, version, acquire, &plan, false, None);
        let fill = self.fills.get_mut(&id).expect("present");
        fill.probe = Some(pid);
        Ok(())
    }

    /// Send one planned probe for fill `id` to `dest`. Returns the probe id.
    #[allow(clippy::too_many_arguments)]
    fn launch(
        &mut self,
        id: u64,
        dest: u32,
        version: u64,
        acquire: bool,
        plan: &AttemptPlan,
        direct: bool,
        hedge: Option<bool>,
    ) -> u64 {
        let now = self.now();
        let fill = &self.fills[&id];
        let deadline = plan.overall.saturating_duration_since(self.base).as_nanos() as Nanos;
        let node = fill.node;
        let msg = Msg {
            probe: 0,
            requester: node,
            at: node,
            dest,
            version,
            page: fill.page,
            acquire,
            path: vec![node],
            links: plan.links,
            attempts: plan.remote_attempts,
            deadline,
            abandon: self.nanos(plan.attempt_end).max(now + 1),
            relays: Vec::new(),
            relay_first: 0,
            direct,
        };
        let pid = self.fresh();
        let mid = self.fresh();
        self.probes.insert(
            pid,
            Probing {
                fill: id,
                acquire,
                hedge,
            },
        );
        self.msgs.insert(mid, Msg { probe: pid, ..msg });
        let timeout = self.nanos(plan.attempt_end).max(now + 1);
        self.engine.at(timeout, Event::ProbeTimeout { probe: pid });
        self.metrics.incr(if acquire {
            "peer_requests_acquire"
        } else {
            "peer_requests_copy_only"
        });
        self.forward(mid, node, None);
        pid
    }

    // ---- peer messages ----

    /// Route `msg` one hop from `holder`. `plan` is the relay plan at a relay;
    /// `None` at the requester.
    fn forward(&mut self, mid: u64, holder: u32, plan: Option<RelayPlan>) {
        let m = self.msgs.get(&mid).expect("message");
        if m.direct && plan.is_none() {
            // A pinned direct hedge exchange: one link, no route search
            // (production `request_direct`).
            let dest = m.dest;
            return self.link(mid, holder, None, dest);
        }
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
        let blocked = self.blocked(holder, &members);
        let seed = m.probe.to_be_bytes();
        let worker = (m.probe % u64::from(self.fabric.workers)) as u8;
        let paths = self.nodes[holder as usize].paths.clone();
        let held = members.clone();
        let hint = blocked.clone();
        let future: TaskFuture = Box::pin(async move {
            let query = PathQuery {
                from,
                to,
                links,
                visited: &visited,
                blocked: &hint,
                seed: &seed,
            };
            paths.route(&*held, query).await
        });
        self.spawn(
            holder,
            worker,
            future,
            members,
            true,
            Then::Route {
                msg: mid,
                holder,
                plan,
                blocked,
            },
        );
    }

    /// Links from `holder` its circuits currently block (the path hint).
    fn blocked(&self, holder: u32, members: &Members) -> Vec<usize> {
        let instant = self.instant(self.now());
        let n = &self.nodes[holder as usize];
        n.suspects
            .iter()
            .filter(|s| !n.links.available(s, instant))
            .filter_map(|s| position(members, *s))
            .collect()
    }

    /// A route search for `mid` at `holder` finished.
    fn routed(
        &mut self,
        mid: u64,
        holder: u32,
        plan: Option<RelayPlan>,
        blocked: Vec<usize>,
        members: Rc<Members>,
        route: Result<Vec<usize>, topology::Error>,
    ) {
        if !self.msgs.contains_key(&mid) {
            return;
        }
        if self.nodes[holder as usize].down {
            return;
        }
        // Production `Paths::shortest` (`src/topology.rs`): link health may
        // change while the search yields; a changed blocked set fails the
        // attempt with `Unavailable` under the same budget.
        if self.blocked(holder, &members) != blocked {
            self.metrics.incr("route_blocked_changed");
            self.note_cut(mid, Cut::Route);
            return self.fail_hop(mid, holder, Fault::Unavailable);
        }
        let route = match route {
            Ok(route) => route,
            Err(topology::Error::Overloaded) => {
                return self.fail_hop(mid, holder, Fault::Overloaded);
            }
            Err(_) => {
                self.metrics.incr("route_unreachable");
                self.note_cut(mid, Cut::Route);
                return self.fail_hop(mid, holder, Fault::Unavailable);
            }
        };
        let next = members.members()[route[1]].node;
        self.link(mid, holder, plan, next);
    }

    /// Send `mid` from `holder` over its link to `next`.
    fn link(&mut self, mid: u64, holder: u32, plan: Option<RelayPlan>, next: u32) {
        let now = self.now();
        let instant = self.instant(now);
        let m = self.msgs.get_mut(&mid).expect("message");
        if let Some(plan) = plan {
            let Ok(outbound) = plan.outbound else {
                self.note_cut(mid, Cut::Route);
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
            self.note_cut(mid, Cut::Circuit);
            return self.fail_hop(mid, holder, Fault::Unavailable);
        };
        drop(probe);
        let site = |x: u32| self.fabric.site[x as usize];
        let partitioned = self.partitions.blocked(now, site(holder), site(next));
        let cut = if self.nodes[next as usize].down {
            Some(Cut::Down)
        } else if !self.versions.enrolled(next) {
            Some(Cut::NotServing)
        } else if partitioned {
            Some(Cut::Partition)
        } else {
            None
        };
        let up = cut.is_none();
        if let Some(cut) = cut {
            self.note_cut(mid, cut);
        }
        let target = &self.nodes[next as usize];
        let unready =
            (!target.down && !self.versions.enrolled(next)).then_some(target.incarnation > 0);
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
            if partitioned {
                self.metrics.incr("partition_link_failures");
            }
            if let Some(restarted) = unready {
                // A member that has not applied a membership yet: no peer
                // listener (Known issue 14).
                self.metrics.incr("unready_member_links");
                if restarted {
                    self.metrics.incr("unready_member_links_restarted");
                }
            }
            if let Some(since) = self.down_since.get(&next) {
                // A peer chose a crashed member: how long after the crash.
                self.metrics.incr("dead_member_links");
                self.metrics
                    .observe("dead_member_link_age_s", (now - since) / SECONDS);
            }
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
    /// Record why walk probe message `mid` failed in transport (the first
    /// cause wins).
    fn note_cut(&mut self, mid: u64, cut: Cut) {
        let Some(m) = self.msgs.get(&mid) else {
            return;
        };
        let Some(p) = self.probes.get(&m.probe) else {
            return;
        };
        if p.hedge.is_none()
            && let Some(fill) = self.fills.get_mut(&p.fill)
            && fill.cut.is_none()
        {
            fill.cut = Some(cut);
        }
    }

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
        if self.nodes[at as usize].down || !self.versions.enrolled(at) {
            self.metrics.incr("messages_lost_to_crash");
            let cut = if self.nodes[at as usize].down {
                Cut::Down
            } else {
                Cut::NotServing
            };
            self.note_cut(mid, cut);
            self.msgs.remove(&mid);
            return;
        }
        if now >= m.abandon {
            // The requester gave up: production relays and the destination
            // see the upstream connection close and drop the exchange.
            self.metrics.incr("requests_abandoned");
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
                        let m = self.msgs.get_mut(&mid).expect("message");
                        if m.relays.is_empty() {
                            m.relay_first = now;
                        }
                        m.relays.push(charge);
                        self.forward(mid, at, Some(plan));
                    }
                    Err(_) => self.fail_hop(mid, at, Fault::Overloaded),
                }
            }
        }
    }

    /// The member fill `id`'s node asks to acquire its page (production
    /// `CandidatePolicy::fanout_helper`): one of the page's helpers, never an
    /// owner or the node itself, preferring a direct overlay neighbor.
    fn fanout_helper(&self, id: u64) -> Option<u32> {
        let fill = &self.fills[&id];
        let (node, page) = (fill.node, fill.page);
        let members = &self.versions.get(fill.version)?.members;
        let me = position(members, node);
        let at = |p: usize| members.members()[p].node;
        fanout_helper(
            &self.ids.object(page),
            self.ids.number(page),
            self.ids.node(node),
            members.members().len(),
            |p| at(p) == node || fill.cands.contains(&at(p)),
            |p| me.is_some_and(|me| members.is_neighbor(me, p)),
        )
        .map(at)
    }

    /// Bodies of one page an owner streams before it may shed (production
    /// `hot_page_threshold`): `hot_page_bodies` of the simulated page size,
    /// unless `node.hot_page_bodies` overrides it.
    fn hot_page_threshold(&self) -> usize {
        match self.config.node.hot_page_bodies {
            0 => hot_page_bodies(self.config.page_bytes),
            n => n as usize,
        }
    }

    /// Production hot-page shed (`racer_core::read::fanout::shed_hot_page`):
    /// an owner already streaming [`Self::hot_page_threshold`] bodies of `page`, at
    /// least half of all bodies it streams, answers `Overloaded` to a
    /// requester that is neither an owner nor a helper of the page.
    fn shed_hot(&mut self, dest: u32, requester: u32, page: u64) -> bool {
        let now = self.now();
        let streaming = match self.streaming.get_mut(&(dest, page)) {
            Some(ends) => {
                ends.retain(|end| *end > now);
                let n = ends.len();
                if n == 0 {
                    self.streaming.remove(&(dest, page));
                }
                n
            }
            None => 0,
        };
        let threshold = self.hot_page_threshold();
        if streaming < threshold || !self.config.node.fanout {
            return false;
        }
        let all = match self.bodies.get_mut(&dest) {
            Some(ends) => {
                ends.retain(|end| *end > now);
                let n = ends.len();
                if n == 0 {
                    self.bodies.remove(&dest);
                }
                n
            }
            None => 0,
        };
        // Production ranks the page here (`PeerServer::shed_hot_page`,
        // `placement.rank`), not only from the placement cache: an owner that
        // holds the page without having ranked it under its current
        // membership (fetched under an older one, or never missed it since)
        // still sheds (Known issue 23).
        let w = self.ids.worker(page);
        let owners = self.owners_at(dest, w, page, true);
        if !owners.contains(&dest) {
            return false;
        }
        let version = self.nodes[dest as usize].version;
        let Some(members) = self.versions.get(version).map(|v| v.members.clone()) else {
            return false;
        };
        let exempt = owners.contains(&requester)
            || position(&members, requester).is_some_and(|p| {
                is_helper(
                    &self.ids.object(page),
                    self.ids.number(page),
                    p,
                    members.members().len(),
                )
            });
        shed_hot_page(true, streaming, all, exempt, threshold)
    }

    fn serve(&mut self, mid: u64) {
        let now = self.now();
        let m = self.msgs.get(&mid).expect("message");
        let (dest, page, requester) = (m.dest, m.page, m.requester);
        if self.shed_hot(dest, requester, page) {
            self.metrics.incr("hot_page_sheds");
            let reply = peer_error(Error::Overloaded).map_or(Reply::Transport, Reply::Response);
            self.send_reply(mid, dest, reply, now);
            return;
        }
        let m = self.msgs.get(&mid).expect("message");
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
                    self.note_hint(dest, page, requester, true);
                    self.send_reply(mid, dest, Reply::Response(ResponseKind::Miss), t);
                }
            }
            ServeAction::Acquire if self.defer_to_successor(dest, page, requester) => {
                // Another owner fetched this page after this node answered
                // it without a copy: let the requester ask it (Known issue
                // 21) instead of fetching again.
                self.metrics.incr("successor_deferrals");
                self.send_reply(mid, dest, Reply::Response(ResponseKind::Miss), now);
            }
            ServeAction::Acquire => {
                let m = self.msgs.get(&mid).expect("message");
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
        let reply = if from == m.dest && reply == Reply::Response(ResponseKind::Overloaded) {
            Reply::Shed
        } else {
            reply
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
            // The body streams only while the requester still waits; relays
            // hold their `Relay` reservation until the body has passed
            // (production keeps it on the connection while streaming,
            // `peer/server.rs` `relay.reservation`).
            let done = self.fabric.transfer_until(sealed, &route, bytes, m.abandon);
            let release = done.unwrap_or(m.abandon);
            if !m.relays.is_empty() {
                // How long the first relay holds its `Relay` reservation:
                // waiting for the destination's answer, then streaming.
                self.metrics
                    .observe("relay_hold_wait_ns", ready.saturating_sub(m.relay_first));
                self.metrics
                    .observe("relay_hold_stream_ns", release.saturating_sub(ready));
            }
            if !m.relays.is_empty() && release > now {
                let id = self.fresh();
                self.held.insert(id, m.relays);
                self.engine.at(release, Event::Release { id });
            }
            let Some(done) = done else {
                self.metrics.incr("replies_abandoned");
                return;
            };
            if done > now {
                self.streaming.entry((from, m.page)).or_default().push(done);
                self.bodies.entry(from).or_default().push(done);
            }
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
            Reply::Shed => Ok(Outcome::Shed),
            Reply::Response(ResponseKind::StaleMembership)
                if probe.hedge.is_none() && self.pending_no_refresh(probe.fill) =>
            {
                // A stale handoff target is only a miss, and a predecessor
                // asked under the previous membership no longer resolves it
                // (no adopt, no skip).
                Ok(Outcome::StaleMembership)
            }
            Reply::Response(ResponseKind::StaleMembership) if probe.hedge.is_none() => {
                // Production `resolve_epoch_traced`: refresh only to a strictly
                // newer membership, once; otherwise the destination is behind
                // and the walk moves on (Known issue 3).
                let (node, retried, previous) = (fill.node, fill.retried, fill.version);
                let owner = fill.rank.is_some();
                let latest = self.adopt(node);
                match stale_action(retried, previous, Some(latest)) {
                    Ok(StaleAction::Refresh) => Ok(Outcome::StaleMembership),
                    Ok(StaleAction::Skip) => {
                        self.metrics.incr("stale_destination_skips");
                        if let Some(fill) = self.fills.get_mut(&probe.fill) {
                            fill.stale += 1;
                        }
                        if owner {
                            // An owner's predecessor is behind (Known issue 12).
                            self.metrics.incr("owner_stale_destination_skips");
                        }
                        Ok(Outcome::StaleDestination)
                    }
                    Err(e) => Err(e),
                }
            }
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
        match probe.hedge {
            None => self.complete_probe(probe.fill, outcome),
            Some(primary) => {
                // Production `hedge_page` contenders: a page wins, a stale
                // reply marks the race stale, any other answer is a
                // recoverable `Unavailable` (the primary's is kept as the
                // walk's first evidence).
                let result = match outcome {
                    Ok(Outcome::Copy) => Ok(()),
                    Ok(Outcome::StaleMembership) => {
                        self.set_race(probe.fill, |r| r.stale = true);
                        Err(Error::Unavailable)
                    }
                    Ok(Outcome::Shed) => {
                        if primary {
                            self.set_race(probe.fill, |r| {
                                r.primary_outcome = Some(ProbeOutcome::Overloaded)
                            });
                        }
                        Err(Error::Unavailable)
                    }
                    Ok(Outcome::Probe(o)) => {
                        if primary {
                            self.set_race(probe.fill, |r| r.primary_outcome = Some(o));
                        }
                        Err(Error::Unavailable)
                    }
                    Ok(Outcome::Transport(_) | Outcome::StaleDestination) => {
                        Err(Error::Unavailable)
                    }
                    Err(e) => Err(e),
                };
                self.hedge_result(probe.fill, primary, result);
            }
        }
    }

    fn set_race(&mut self, id: u64, f: impl FnOnce(&mut Race)) {
        if let Some(race) = self.fills.get_mut(&id).and_then(|f| f.hedge.as_mut()) {
            f(race);
        }
    }

    /// Whether a `StaleMembership` answer to `fill`'s pending candidate probe
    /// must not refresh membership: a handoff target, or a predecessor asked
    /// under the previous membership.
    fn pending_no_refresh(&self, fill: u64) -> bool {
        self.fills.get(&fill).is_some_and(|f| {
            matches!(&f.walk, Walking::Candidates(walk)
                if walk.pending_handoff() || walk.pending_previous())
        })
    }

    fn complete_probe(&mut self, id: u64, outcome: Result<Outcome, Error>) {
        let Some(fill) = self.fills.get_mut(&id) else {
            return;
        };
        fill.probe = None;
        if let Walking::Candidates(walk) = &fill.walk
            && walk.pending_helper()
        {
            let label = match &outcome {
                Ok(Outcome::Copy) => "helper_copies",
                Ok(Outcome::Transport(ProbeOutcome::Unreachable)) => "helper_unreachable",
                Ok(Outcome::Transport(_) | Outcome::Probe(ProbeOutcome::Overloaded)) => {
                    "helper_overloaded"
                }
                Ok(Outcome::Shed) => "helper_shed",
                Ok(_) => "helper_misses",
                Err(_) => "helper_errors",
            };
            self.metrics.incr(label);
        }
        if let Some(dest) = fill.asking.take() {
            let asked = match &outcome {
                Ok(Outcome::Copy) => Asked::Copy,
                Ok(Outcome::Probe(ProbeOutcome::CopyMiss | ProbeOutcome::VersionUnavailable)) => {
                    Asked::Miss
                }
                Ok(Outcome::Transport(ProbeOutcome::Unreachable)) => {
                    Asked::Unreachable(fill.cut.take().unwrap_or(Cut::Other))
                }
                Ok(Outcome::Probe(ProbeOutcome::Unreachable)) => Asked::Failed,
                Ok(_) => Asked::Busy,
                Err(_) => Asked::Unfunded,
            };
            fill.asked.push((dest, asked));
        }
        // `reply` counted a skipped destination that is behind this node.
        let skipped = outcome == Ok(Outcome::StaleDestination) && fill.stale > 0;
        match &mut fill.walk {
            Walking::Candidates(walk) => match outcome {
                Err(e) => self.hard_fail(id, e),
                Ok(outcome) => match walk.complete(&mut fill.budget, outcome) {
                    Err(e) => self.hard_fail(id, e),
                    Ok(Some(end)) => self.on_end(id, end),
                    Ok(None) if skipped && self.stale_fallback(id) => {}
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

    /// Production `resolve_epoch_traced`: at a destination behind this
    /// node, a non-owner stops its walk and walks the page's owners under
    /// membership version - 1 instead, once (`racer_core::read::stale`,
    /// Known issue 19). Returns whether it did.
    fn stale_fallback(&mut self, id: u64) -> bool {
        let now = self.now();
        let fill = &self.fills[&id];
        let (node, w, page, version) = (fill.node, fill.worker, fill.page, fill.version);
        let old = version
            .checked_sub(1)
            .and_then(|v| self.versions.get(v))
            .map(|v| v.members.clone());
        // As for owners' previous probes (`walk_ranked`): replaced within
        // `PREVIOUS_MEMBERSHIP_WITHIN`, measured from the publication.
        let recent = self.config.node.retained_versions >= 1
            && self.versions.get(version).is_some_and(|v| {
                now.saturating_sub(v.published) <= PREVIOUS_MEMBERSHIP_WITHIN.as_nanos() as Nanos
            });
        let member = old.as_ref().is_some_and(|m| position(m, node).is_some());
        let go = previous_fallback(StaleSkip {
            owner: fill.rank.is_some(),
            retried: fill.retried || fill.fell_back,
            previous_recent_member: recent && member,
        }) && !fill.fallback_denied
            && self.config.node.stale_fallback;
        let (true, Some(old)) = (go, old) else {
            // Why a non-owner could not fall back (Known issue 19).
            if fill.rank.is_none() && !fill.retried && !fill.fell_back {
                self.metrics.incr(if !member {
                    "stale_fallback_not_member"
                } else if !recent {
                    "stale_fallback_not_recent"
                } else {
                    "stale_fallback_other"
                });
            }
            return false;
        };
        // The skipped probe completed, so nothing is pending; the walk
        // under the current membership waits for the ranking.
        self.fills.get_mut(&id).expect("present").fell_back = true;
        self.rank(
            node,
            w,
            old,
            page,
            Then::Fallback {
                fill: id,
                version: version - 1,
            },
        );
        true
    }

    /// The page is ranked under the previous membership: walk it there, or,
    /// when this node owns it there, keep walking the current membership.
    fn fallback_ranked(
        &mut self,
        id: u64,
        version: u64,
        members: Rc<Members>,
        cands: Result<Vec<u32>, Error>,
    ) {
        let Some(fill) = self.fills.get_mut(&id) else {
            return;
        };
        let allowed = match &cands {
            Ok(c) if !c.is_empty() => fallback_rank_allowed(c.iter().position(|n| *n == fill.node)),
            _ => false,
        };
        if !allowed {
            self.metrics.incr("stale_fallback_denied");
            fill.fell_back = false;
            fill.fallback_denied = true;
            return self.step_walk(id);
        }
        self.metrics.incr("stale_fallbacks");
        fill.walk = Walking::None;
        fill.retried = true;
        // A fresh walk: the previous membership's owners were not asked.
        fill.continuation = Continuation::default();
        self.walk_ranked(id, version, members, cands);
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
                match stale_action(retried, previous, Some(latest)) {
                    Ok(StaleAction::Refresh) => {
                        self.metrics.incr("walk_membership_refreshes");
                        self.fills.get_mut(&id).expect("present").retried = true;
                        return self.begin_walk(id);
                    }
                    // `reply` already turned a skip into `StaleDestination`.
                    Ok(StaleAction::Skip) => {
                        let fill = self.fills.get_mut(&id).expect("present");
                        fill.plan
                            .walk(WalkResult::Failed(Error::IncompatibleMembership));
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
                if fill.stale > 0 {
                    self.metrics.incr("walk_exhausted_stale");
                }
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
        let fill = self.fills.get(&id).expect("present");
        let (node, w, page, version) = (fill.node, fill.worker, fill.page, fill.version);
        let Some(members) = self.versions.get(version).map(|v| v.members.clone()) else {
            return self.fail_walk(id, Error::IncompatibleMembership);
        };
        self.rank(
            node,
            w,
            members,
            page,
            Then::Origin {
                fill: id,
                rank,
                evidence,
            },
        );
    }

    fn origin_ranked(
        &mut self,
        id: u64,
        rank: usize,
        evidence: Vec<ProbeOutcome>,
        members: Rc<Members>,
        expected: Result<Vec<u32>, Error>,
    ) {
        let now = self.now();
        let Some(fill) = self.fills.get(&id) else {
            return;
        };
        let (node, page, version) = (fill.node, fill.page, fill.version);
        let cands = fill.cands.clone();
        let expected = match expected {
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
        if version < self.versions.latest_number() {
            self.metrics.incr("origin_on_older_membership");
        }
        let instant = self.instant(now);
        let fill = self.fills.get_mut(&id).expect("present");
        // Production debits one attempt before origin
        // (`src/read/fill.rs`, `budget.begin_attempt` after
        // `authority.validate`); an owner without one fails the fill.
        let scope = fill.scope;
        if let Err(e) = fill.budget.begin_attempt(instant, scope) {
            self.metrics.incr("origin_unfunded");
            return self.fail_walk(id, e);
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
        let (handle, done, outcome) =
            self.origin
                .request_tracked(now, object, self.config.page_bytes, errors, id);
        self.metrics.incr("origin_requests");
        *self.origin_per_page.entry(page).or_default() += 1;
        self.classify_repeat(id);
        let fill = self.fills.get_mut(&id).expect("present");
        fill.origin = Some((handle, outcome));
        // Production bounds the origin exchange by the deadline of the waiter
        // the fill currently serves, then hands it over or cancels it
        // (`src/read/fill.rs`, `FlightOperation::poll_cancellation`).
        if let Some(check) = self.origin_check_at(id)
            && done > check
        {
            self.engine.at(check, Event::OriginCancel { fill: id });
        }
        self.engine
            .at(done, Event::OriginDone { fill: id, outcome });
    }

    /// Close an outstanding origin request now and move the completions of
    /// the requests queued behind it.
    fn cancel_origin(&mut self, handle: OriginHandle) {
        let now = self.engine.now();
        if self.origin.cancel(handle, now) > 0 {
            self.schedule_origin_wake();
        }
    }

    fn schedule_origin_wake(&mut self) {
        if let Some(next) = self.origin.next_moved()
            && self.origin_wake.is_none_or(|w| next < w)
        {
            self.origin_wake = Some(next);
            self.engine.at(next, Event::OriginWake);
        }
    }

    fn origin_wake(&mut self) {
        let now = self.engine.now();
        if self.origin_wake == Some(now) {
            self.origin_wake = None;
        }
        for id in self.origin.pop_moved(now) {
            self.origin_complete(id);
        }
        self.schedule_origin_wake();
    }

    /// The fill's scope expired with its origin request outstanding: close
    /// it (freeing origin concurrency and bandwidth for later requests) and
    /// fail the fill with `DeadlineExceeded`, as production does.
    fn origin_cancel(&mut self, id: u64) {
        let now = self.now();
        let Some(fill) = self.fills.get(&id) else {
            return;
        };
        let Some((handle, _)) = fill.origin else {
            return;
        };
        let Some(done) = self.origin.end(handle).filter(|done| *done > now) else {
            return;
        };
        if let Some(check) = self.hand_over(id) {
            if done > check {
                self.engine.at(check, Event::OriginCancel { fill: id });
            }
            return;
        }
        let fill = self.fills.get_mut(&id).expect("present");
        fill.origin = None;
        fill.canceled = true;
        let (node, page) = (fill.node, fill.page);
        fill.plan.origin(Err(Error::DeadlineExceeded));
        self.metrics.incr("origin_canceled");
        self.cancel_origin(handle);
        if let Some(last) = self.last_origin.get_mut(&page)
            && last.node == node
            && last.ok.is_none()
        {
            last.ok = Some(false);
            last.done = now;
        }
        self.continue_plan(id);
    }

    /// When the fill's origin request must next be checked: the deadline of
    /// the waiter it serves, capped by the fill's ceiling. `None` once no
    /// live waiter remains.
    fn origin_check_at(&self, id: u64) -> Option<Nanos> {
        let fill = &self.fills[&id];
        let flights = &self.nodes[fill.node as usize].workers[fill.worker as usize].flights;
        let deadline = flights.leader_deadline(&self.ids.page(fill.page))?;
        Some(self.nanos(deadline.min(fill.scope)))
    }

    /// The waiter fill `id` serves reached its deadline. Refresh the flight
    /// so another live waiter adopts the fill (Known issue 30). If one did,
    /// fail the elected leader's own read, which production ends at its
    /// deadline while its driver keeps running, and return when to check
    /// again; `None` cancels the fill.
    fn hand_over(&mut self, id: u64) -> Option<Nanos> {
        let now = self.now();
        let instant = self.instant(now);
        let fill = &self.fills[&id];
        let (node, w, page, leader, token, ceiling) = (
            fill.node,
            fill.worker,
            fill.page,
            fill.leader,
            fill.token,
            self.nanos(fill.scope),
        );
        if now >= ceiling {
            return None;
        }
        let page_id = self.ids.page(page);
        let mut wakes = Vec::new();
        let flights = &mut self.nodes[node as usize].workers[w as usize].flights;
        flights.refresh(&page_id, instant, &mut wakes);
        let live = flights
            .leader_deadline(&page_id)
            .filter(|deadline| *deadline > instant);
        for token in wakes {
            self.wake(token);
        }
        live?;
        self.metrics.incr("flight_handovers");
        if let Some(waiting) = self.tokens.remove(&token) {
            self.detach(node, w, page, leader);
            self.resolve(waiting.want, Err(Error::DeadlineExceeded));
        }
        self.origin_check_at(id)
    }

    /// Classify a repeated origin request for a page (Known issues 6, 12,
    /// 13, 15). With `A` the node that last asked origin for the page and
    /// `F` the fill asking now:
    ///
    /// - `origin_repeats_after_move`: the page's rank 0 differs from rank 0
    ///   under `A`'s request, so ownership moved in between. At most one
    ///   such fetch per move is the bound the walk aims for: the new rank 0
    ///   finds `A` only through handoff probes (previous membership) or not
    ///   at all once that membership is gone.
    /// - `origin_repeats_after_failure`: same rank 0, and `A`'s request
    ///   failed (origin 5xx, 412, 403), so nobody got the page from it.
    /// - `origin_repeats_concurrent`: same rank 0, and `A`'s request is
    ///   still in flight (two cold misses that did not coalesce).
    /// - `origin_repeats_copy_lost`: same rank 0, `A`'s request succeeded,
    ///   and `A` no longer holds the page (evicted or crashed).
    /// - `origin_repeats_copy_held`: same rank 0 and `A` still holds the
    ///   page, split by why `F` did not get it from `A`
    ///   (`origin_repeats_held_*`):
    ///   - `raced`: `A`'s copy landed after `F` started, so `F` may have
    ///     asked before it existed (concurrency);
    ///   - `unreachable`: `F` asked `A` and the probe failed in transport
    ///     (`A` or the route to it down, partitioned, not serving yet);
    ///   - `busy`: `F` asked `A` and it, or a relay, shed the request or
    ///     was behind on membership;
    ///   - `unfunded`: `F` had a probe for `A` but no budget to send it;
    ///   - `missed`: `F` asked `A` and was told it had no copy although
    ///     it held one before `F` started (should not happen);
    ///   - `unasked`: `F` never asked `A`. This is the avoidable class
    ///     (Known issue 15): owners ask predecessors, handoff targets and
    ///     successors, so `A` goes unasked only when it is no current or
    ///     previous owner, or the walk ran out of credits first.
    fn classify_repeat(&mut self, id: u64) {
        let now = self.now();
        let fill = &self.fills[&id];
        let (node, page) = (fill.node, fill.page);
        let rank0 = fill.cands.first().copied().unwrap_or(node);
        // An owner above rank 0 reaches origin only with transient evidence
        // against rank 0: record why rank 0 did not serve (the last probe of
        // rank 0 decides).
        let skip = (fill.rank.is_some_and(|r| r > 0)).then(|| {
            let last = fill
                .asked
                .iter()
                .rev()
                .find(|(dest, _)| *dest == rank0)
                .map(|(_, a)| *a);
            match last {
                Some(Asked::Unreachable(cut)) => match cut {
                    Cut::NotServing => "not_serving",
                    Cut::Partition => "partition",
                    Cut::Down => "down",
                    Cut::Route => "route",
                    Cut::Circuit => "circuit",
                    Cut::Other => "timeout",
                },
                Some(Asked::Failed) => "failed",
                Some(Asked::Busy) => "busy",
                Some(Asked::Miss) => "copy_only",
                Some(Asked::Copy) => "copy",
                Some(Asked::Unfunded) | None => "unfunded",
            }
        });
        if let Some(skip) = skip {
            self.metrics.incr(skip_counter("origin_skips_", skip));
        }
        let next = LastOrigin {
            node,
            rank0,
            ok: None,
            done: now,
            skip,
            version: fill.version,
        };
        let Some(last) = self.last_origin.insert(page, next) else {
            return;
        };
        let held = |n: &NodeWorld| {
            let holder = &n.nodes[last.node as usize];
            !holder.down
                && (holder.disk.entries.get(&page).is_some()
                    || holder
                        .workers
                        .iter()
                        .any(|w| w.memory.pages.contains_key(&page)))
        };
        let class = if last.rank0 != rank0 {
            // Why a move repeats (Known issue 20): the earlier fetcher lost
            // the copy, or still holds it and this owner did or did not ask
            // it; and whether the page moved over one version or more.
            let sub = if !held(self) {
                "origin_repeats_after_move_lost"
            } else if let Some(&(_, a)) = fill
                .asked
                .iter()
                .rev()
                .find(|&&(dest, _)| dest == last.node)
            {
                match a {
                    Asked::Miss => "origin_repeats_after_move_held_asked_miss",
                    Asked::Copy => "origin_repeats_after_move_held_asked_copy",
                    Asked::Unreachable(_) | Asked::Failed => {
                        "origin_repeats_after_move_held_asked_unreachable"
                    }
                    Asked::Busy => "origin_repeats_after_move_held_asked_busy",
                    Asked::Unfunded => "origin_repeats_after_move_held_asked_unfunded",
                }
            } else if fill.version == last.version + 1 && fill.handoff.contains(&last.node) {
                "origin_repeats_after_move_held_unasked_target"
            } else if fill.version == last.version + 1 && fill.cands.contains(&last.node) {
                "origin_repeats_after_move_held_unasked_owner"
            } else if fill.version == last.version {
                "origin_repeats_after_move_held_unasked_same"
            } else if fill.version > last.version + 2 {
                "origin_repeats_after_move_held_unasked_older"
            } else if fill.version == last.version + 2 {
                "origin_repeats_after_move_held_unasked_two"
            } else {
                "origin_repeats_after_move_held_unasked"
            };
            self.metrics.incr(sub);
            "origin_repeats_after_move"
        } else if last.ok == Some(false) {
            "origin_repeats_after_failure"
        } else if last.ok.is_none() {
            // Known issue 31: who overlaps whom. `F` above rank 0 skipped
            // rank 0 (why), or `F` is rank 0 and `A` skipped it earlier.
            let sub = if last.node == node {
                "origin_repeats_concurrent_same_node"
            } else if let Some(skip) = skip {
                concurrent_counter(skip)
            } else if last.skip.is_some() {
                "origin_repeats_concurrent_rank0_after_skip"
            } else {
                "origin_repeats_concurrent_other"
            };
            self.metrics.incr(sub);
            self.fills.get_mut(&id).expect("present").concurrent = true;
            "origin_repeats_concurrent"
        } else if !held(self) {
            "origin_repeats_copy_lost"
        } else {
            let asked = |kind: fn(Asked) -> bool| {
                fill.asked
                    .iter()
                    .any(|&(dest, a)| dest == last.node && kind(a))
            };
            let cause = if last.done > fill.started {
                "origin_repeats_held_raced"
            } else if asked(|a| matches!(a, Asked::Unreachable(_) | Asked::Failed)) {
                "origin_repeats_held_unreachable"
            } else if asked(|a| a == Asked::Busy) {
                "origin_repeats_held_busy"
            } else if asked(|a| a == Asked::Unfunded) {
                "origin_repeats_held_unfunded"
            } else if asked(|a| matches!(a, Asked::Miss | Asked::Copy)) {
                "origin_repeats_held_missed"
            } else {
                "origin_repeats_held_unasked"
            };
            self.metrics.incr(cause);
            // `A` fetched as an owner above rank 0, so rank 0 has no copy
            // and fetches again: charge the repeat to why `A` skipped it.
            if let Some(skip) = last.skip {
                self.metrics
                    .incr(skip_counter("origin_repeats_orphan_", skip));
            }
            // Every such repeat is a skip itself (`F` above rank 0), an
            // orphan (`A` fetched as a skip), or raced. Anything else is
            // the avoidable class (rank 0 fetching although the copy it
            // or its delegate fetched is still held); tests keep it at 0.
            let explained =
                skip.is_some() || last.skip.is_some() || cause == "origin_repeats_held_raced";
            if !explained {
                self.metrics.incr("origin_repeats_held_unexplained");
            }
            self.invariants.check(
                "origin_repeat_explained",
                explained,
                now,
                Some(node),
                || {
                    format!(
                        "page {page} fetched by {node} while {} still holds it ({cause})",
                        last.node
                    )
                },
            );
            "origin_repeats_copy_held"
        };
        self.metrics.incr(class);
    }

    /// The completion event scheduled at issue. Stale if the request was
    /// canceled or a cancellation moved it ([`Self::origin_wake`]).
    fn origin_done(&mut self, id: u64) {
        let now = self.engine.now();
        if let Some((handle, _)) = self.fills.get(&id).and_then(|f| f.origin)
            && self.origin.due_as_issued(handle, now)
        {
            self.origin_complete(id);
        }
    }

    fn origin_complete(&mut self, id: u64) {
        let now = self.engine.now();
        let Some(fill) = self.fills.get_mut(&id) else {
            return;
        };
        let Some((handle, outcome)) = fill.origin.take() else {
            return;
        };
        self.origin.finish(handle);
        self.metrics.incr("origin_answered");
        let (scope, node) = (fill.scope, fill.node);
        let scope = self.nanos(scope);
        self.invariants
            .check("origin_within_scope", now <= scope, now, Some(node), || {
                format!("fill {id} origin answered at {now} after its scope {scope}")
            });
        // Known issue 30: a handed-over fill runs only while some waiter
        // it serves is live.
        let live = self.origin_check_at(id).is_some_and(|check| now <= check);
        self.invariants
            .check("origin_for_live_waiter", live, now, Some(node), || {
                format!("fill {id} origin answered at {now} with no live waiter")
            });
        let fill = self.fills.get_mut(&id).expect("present");
        let result = match outcome {
            OriginOutcome::Ok => {
                self.metrics.add("origin_bytes", self.config.page_bytes);
                if fill.concurrent {
                    // Known issue 31: overlapping requests that cost egress.
                    self.metrics.incr("origin_concurrent_completed");
                }
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
        let (node, page) = (fill.node, fill.page);
        let ok = result.is_ok();
        fill.plan.origin(result);
        let now = self.now();
        if let Some(last) = self.last_origin.get_mut(&page)
            && last.node == node
            && last.ok.is_none()
        {
            last.ok = Some(ok);
            last.done = now;
        }
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
        let fill = self.fills.get(&id).expect("present");
        let (node, w, page, eligible) = (fill.node, fill.worker, fill.page, fill.eligible);
        self.memory_insert(node, w, page);
        self.persist_page(node, w, page, eligible);
        let fill = self.fills.get_mut(&id).expect("present");
        fill.plan.published(Ok(()));
        let _ = source;
        self.continue_plan(id);
    }

    /// Whether `node` owns `page` in its current published snapshot, from
    /// the worker's cached ranking only (production `Retention::is_owned`:
    /// missing or partial rankings are not owned).
    fn owned(&self, node: u32, w: u8, page: u64) -> bool {
        let n = &self.nodes[node as usize];
        let Some(version) = self.versions.get(n.version) else {
            return false;
        };
        let Some(me) = position(&version.members, node) else {
            return false;
        };
        n.workers[w as usize]
            .placement
            .is_owner(&*version.members, &page.to_be_bytes(), |o| o == me)
    }

    /// Production `Retention::observe_at` for one client operation: the
    /// ownership predicate, then second-sight history and heat.
    fn observe(&mut self, node: u32, w: u8, page: u64) -> bool {
        let owned = self.owned(node, w, page);
        let instant = self.instant(self.now());
        let retention = &mut self.nodes[node as usize].retention;
        let seen = retention.history.observe(&page, instant);
        retention.heat.touch(&page, instant);
        self.metrics.incr("retention_observations");
        let eligible = retention_eligible(owned, true, seen);
        if eligible {
            self.metrics.incr("retention_qualified");
        }
        eligible
    }

    /// Production `persist_verified`: write an eligible, not yet resident
    /// page, with ownership reclassified now and the observation latched.
    fn persist_page(&mut self, node: u32, w: u8, page: u64, eligible: bool) {
        let owned = self.owned(node, w, page);
        let n = &self.nodes[node as usize];
        let quotas = &n.workers[w as usize].quotas;
        let decision = persist(PersistFacts {
            eligible,
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
        if let Some((handle, ..)) = fill.origin {
            self.cancel_origin(handle);
        }
        for probe in fill.probes() {
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
        let mut wakes = Vec::new();
        let worker = &mut n.workers[fill.worker as usize];
        // Production completes the retained operation first, then publishes
        // and ignores `StaleFlight` (`src/read/fill.rs`, `operation.complete()`
        // and `let _ = flights.publish_acquired(..)`): a leader revoked while
        // its fill ran (canceled or past its deadline) cannot settle the
        // flight, which already drained to a retry once the fill completed.
        let mut admit = Admit {
            quotas: &worker.quotas,
            charges: &mut worker.charges,
            ids: &self.ids,
        };
        if fill.canceled {
            // Production cancels the fill because the leader's scope expired
            // (`Operation::cancellation_requested`) and completes the
            // operation with the clock at that time, so the table sees the
            // leader revoked; the sim's table clock only advances on joins
            // and polls.
            worker
                .flights
                .refresh(&page_id, self.base + Duration::from_nanos(now), &mut wakes);
        }
        let completed = worker
            .flights
            .complete_operation(&page_id, &mut admit, &mut wakes);
        let active = worker.flights.is_leader(&page_id, fill.leader);
        if !active {
            self.metrics.incr("flight_stale_finishes");
        }
        // Same leader settlement as production `Fill` (`src/read/fill.rs`):
        // a credential rejection fails only the leader and re-elects.
        let finished = match result.map_err(racer_core::read::fill::acquisition_failure) {
            _ if !active => Ok(()),
            Ok(_) => worker.flights.finish(
                &page_id,
                fill.leader,
                FlightOutcome::Published(self.config.page_bytes as u32),
                &mut wakes,
            ),
            Err(FailureKind::OriginRejected) => {
                worker
                    .flights
                    .reject(&page_id, fill.leader, Error::OriginRejected, &mut wakes)
            }
            Err(FailureKind::OriginForbidden) => {
                worker
                    .flights
                    .reject(&page_id, fill.leader, Error::OriginForbidden, &mut wakes)
            }
            Err(FailureKind::Terminal(e)) => {
                worker
                    .flights
                    .finish(&page_id, fill.leader, FlightOutcome::Failed(e), &mut wakes)
            }
        };
        self.invariants.check(
            "flight_leader_valid",
            finished.is_ok() && completed.is_ok(),
            now,
            Some(fill.node),
            || {
                format!(
                    "finish page {} leader {}: {finished:?}, operation {completed:?}",
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
        // A rejection both notifies and settles, so a waiter can appear
        // twice; wake each once (a second wake would re-lead it).
        let mut seen = HashSet::new();
        for token in wakes {
            if token != fill.token && seen.insert(token) {
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
                + self.writes.len() * 96
                + self.streaming.len() * 64
                + self.bodies.len() * 64
                + self.tasks.len() * (std::mem::size_of::<Task>() + 256),
        ));
        list.push(("origin_per_page", self.origin_per_page.capacity() * 16));
        list.push(("last_origin", self.last_origin.capacity() * 32));
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
        // Pages by origin request count.
        let mut counts = std::collections::BTreeMap::<u32, u64>::new();
        for c in self.origin_per_page.values() {
            *counts.entry(*c).or_default() += 1;
        }
        let histogram: serde_json::Map<_, _> = counts
            .into_iter()
            .map(|(k, v)| (k.to_string(), json!(v)))
            .collect();
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
                "histogram": histogram,
                "mean_requests_per_page": if pages == 0 { 0.0 } else { requests as f64 / pages as f64 },
                "page_bytes": self.config.page_bytes,
                "bytes_per_page": if pages == 0 { 0 } else { c("origin_bytes") / pages as u64 },
                "repeats": {
                    "after_owner_move": c("origin_repeats_after_move"),
                    "copy_lost": c("origin_repeats_copy_lost"),
                    "after_failure": c("origin_repeats_after_failure"),
                    "concurrent": c("origin_repeats_concurrent"),
                    "copy_held": c("origin_repeats_copy_held"),
                    "held": {
                        "raced": c("origin_repeats_held_raced"),
                        "unreachable": c("origin_repeats_held_unreachable"),
                        "busy": c("origin_repeats_held_busy"),
                        "unfunded": c("origin_repeats_held_unfunded"),
                        "missed": c("origin_repeats_held_missed"),
                        "unasked": c("origin_repeats_held_unasked"),
                    },
                    "unexplained": c("origin_repeats_held_unexplained"),
                    "orphan": SKIPS
                        .iter()
                        .map(|k| (k.to_string(), json!(c(skip_counter("origin_repeats_orphan_", k)))))
                        .collect::<serde_json::Map<_, _>>(),
                },
                "skips": SKIPS
                    .iter()
                    .map(|k| (k.to_string(), json!(c(skip_counter("origin_skips_", k)))))
                    .collect::<serde_json::Map<_, _>>(),
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
                "latest_version": self.versions.latest_number(),
                "publications": c("membership_publications"),
                "placement_memo_entries": self.shared.phantom.memo_len(),
                "route_memo_entries": self.shared.routes.memo_len(),
            },
            "invariants": self.invariants.to_json(),
            "profile_wall_ms": {
                "placement_ranking": self.profile.rank_ns / 1_000_000,
                "route_search": self.profile.route_ns / 1_000_000,
                "membership_build": self.profile.publish_ns / 1_000_000,
            },
            "memory": {
                "components": memory.iter().map(|(k, v)| ((*k).to_owned(), json!(v))).collect::<serde_json::Map<_, _>>(),
                "accounted_bytes": total,
                "accounted_bytes_per_node": total / nodes,
                "peak_rss_bytes": peak_rss_bytes(),
            },
        })
    }

    /// Origin requests for `page` so far.
    /// Origin requests still outstanding (canceled ones excluded).
    pub fn origin_outstanding(&self) -> usize {
        self.origin.outstanding()
    }

    pub fn origin_requests(&self, page: u64) -> u32 {
        self.origin_per_page.get(&page).copied().unwrap_or(0)
    }

    /// Whether worker `w` of `node` holds `page` in memory.
    pub fn in_memory(&self, node: u32, w: u8, page: u64) -> bool {
        self.nodes[node as usize].workers[w as usize]
            .memory
            .pages
            .contains_key(&page)
    }

    /// Whether `node` holds `page` on disk.
    pub fn on_disk(&self, node: u32, page: u64) -> bool {
        self.nodes[node as usize].disk.entries.get(&page).is_some()
    }

    /// The ranked owners of `page` in the latest membership (node ids).
    pub fn owners(&self, page: u64) -> Vec<u32> {
        let members = self.members();
        Placement::<Phantom>::with_rankings(0, self.shared.phantom.clone())
            .rank(&*members, &page.to_be_bytes())
            .into_iter()
            .map(|p| members.members()[p].node)
            .collect()
    }

    /// Shared handles for tests.
    pub fn members(&self) -> Rc<Members> {
        self.versions.latest().members.clone()
    }
}

#[cfg(test)]
mod shed_tests {
    use super::*;

    /// Known issue 23: production `PeerServer::shed_hot_page` ranks the page
    /// (`placement.rank`), so an owner sheds even when its placement cache
    /// holds no ranking of the page under its current membership (it fetched
    /// the page under an older one, or has served it from memory since). The
    /// simulator used the cache-only ownership test of retention, and owners
    /// queued every reader after a membership change.
    #[test]
    fn owner_without_a_cached_ranking_sheds_a_hot_page() {
        let nodes = 256;
        let config = Config {
            nodes,
            request_rate: 0.0,
            pages_per_object: 1,
            ..Config::default()
        };
        let mut world = NodeWorld::new(config, 1);
        let page = 0;
        let w = world.ids.worker(page);
        let members = world
            .versions
            .get(world.nodes[0].version)
            .unwrap()
            .members
            .clone();
        // Rank on a node that is not asked below, so the owners' own caches
        // stay empty.
        let ranker = (0..nodes)
            .find(|n| !world.owners_at(*n, w, page, true).contains(n))
            .unwrap();
        let owners = world.owners_at(ranker, w, page, true);
        assert_eq!(owners.len(), 3);
        let owner = owners[0];
        assert!(world.owners_at(owner, w, page, false).is_empty());
        let helper = |n: u32| {
            position(&members, n).is_some_and(|p| {
                is_helper(
                    &world.ids.object(page),
                    world.ids.number(page),
                    p,
                    members.members().len(),
                )
            })
        };
        let reader = (0..nodes)
            .find(|n| !owners.contains(n) && !helper(*n) && *n != ranker)
            .unwrap();
        let helper_node = (0..nodes).find(|n| !owners.contains(n) && helper(*n));
        let threshold = world.hot_page_threshold();
        let busy = vec![SECONDS; threshold];
        world.streaming.insert((owner, page), busy.clone());
        world.bodies.insert(owner, busy);
        assert!(world.shed_hot(owner, reader, page));
        assert!(!world.shed_hot(owner, owners[1], page), "owners are exempt");
        if let Some(h) = helper_node {
            assert!(!world.shed_hot(owner, h, page), "helpers are exempt");
        }
        // A non-owner never sheds, cached ranking or not.
        let busy = vec![SECONDS; threshold];
        world.streaming.insert((reader, page), busy.clone());
        world.bodies.insert(reader, busy);
        assert!(!world.shed_hot(reader, ranker, page));
        world
            .streaming
            .insert((owner, page), vec![SECONDS; threshold - 1]);
        assert!(!world.shed_hot(owner, reader, page), "below the threshold");
    }
}
