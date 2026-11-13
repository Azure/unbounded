//! The public scenario spec types: pages, workloads, faults and [`ModelSpec`].

use super::*;

/// A page of an object.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct Page {
    /// Object id.
    pub object: u64,
    /// Page number within the object.
    pub number: u32,
}

impl Page {
    pub(super) fn key(self) -> [u8; 12] {
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
    /// The model tracks flight structure, not page bytes.
    type Value = ();

    fn value(_page: &Self::Page, _source: racer_core::read::Source) -> Self::Value {}
}

/// A steady read load (Poisson arrivals and Zipf popularity): every
/// node reads one uniformly drawn page of a Zipf-drawn object at Poisson arrivals of `rate`
/// reads per second, while it is up and a member.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Workload {
    /// Objects, ranked by popularity.
    pub objects: u64,
    /// Zipf exponent.
    pub zipf_s: f64,
    /// Reads per node per second.
    pub rate: f64,
    /// Fraction of reads that read `range_pages` consecutive pages;
    /// the Model issues them as concurrent page reads.
    pub range_fraction: f64,
    /// Pages of a range read, capped at the object's end.
    pub range_pages: u32,
}

/// Origin errors injected during `[from, until)` on top of [`ModelSpec::origin_errors`].
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct OriginBurst {
    /// Start.
    pub from: Nanos,
    /// End.
    pub until: Nanos,
    /// Error rates during the burst.
    pub errors: OriginErrors,
}

/// Every link between site `a` and site `b` is cut during `[from, until)`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SiteCut {
    /// Start.
    pub from: Nanos,
    /// End.
    pub until: Nanos,
    /// One site.
    pub a: u16,
    /// The other site.
    pub b: u16,
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

/// Membership churn: `node` joins or leaves at `at`. A node that left keeps
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

/// Random churn: `events` leave and rejoin pairs, each leave drawn
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
    /// unanswered by then times out (`Reply::TimedOut`). Production uses
    /// [`ATTEMPT_TIMEOUT`], the default; [`NO_ATTEMPT_CAP`] bounds an attempt by its signed
    /// overall deadline only.
    pub attempt_timeout: Nanos,
    /// Pages each node keeps; the least recently used page is evicted past it. 0 is unbounded.
    pub stored_pages: usize,
    /// Injected origin error rates.
    pub origin_errors: OriginErrors,
    /// Cache outages.
    pub outages: Vec<Outage>,
    /// Hung peer servers: during `[at, until)` the node accepts every
    /// peer request, as destination or relay, and never answers or forwards it.
    pub hangs: Vec<Outage>,
    /// Client page reads refresh the object's metadata through the worker's
    /// `MetadataCohort` first.
    pub metadata: bool,
    /// Membership changes, published in batches of [`membership::BATCH`].
    pub churn: Vec<Churn>,
    /// Drawn membership changes, added to [`Self::churn`].
    pub random_churn: Option<RandomChurn>,
    /// Most adoption lag of a node (at most one batch).
    pub membership_lag: Nanos,
    /// `Relay` transfers per worker; production's default unless a scenario overrides it.
    pub relay_transfers: usize,
    /// Crypto jobs one worker may hold before a new request's verification fails `Overloaded`
    /// and the server answers the unsigned overload head;
    /// zero is unbounded.
    pub crypto_queue: usize,
    /// Peer connection slots (`model::connections`): per-worker outbound pools, per-node
    /// ingress, and idle reuse. Production's by default (see `dst/MODEL.md`).
    pub connections: PeerConnections,
    /// Link circuits (production `LinkHealth` in `src/topology.rs`, one per worker): a link
    /// that failed to connect backs off (`racer_core::route::link_backoff`), sends over it fail
    /// at once, and route searches route around it. On by default; off only for ablation.
    pub circuits: bool,
    /// Waiters per flight; production's default unless a scenario overrides it.
    pub waiters_per_flight: usize,
    /// Report checks, by name.
    pub checks: Vec<(String, ReportCheck)>,
    /// The control plane ([`control`]), when the scenario models it.
    pub control: Option<control::ControlSpec>,
    /// Steady Poisson/Zipf reads.
    pub workload: Option<Workload>,
    /// Timed origin error bursts.
    pub origin_bursts: Vec<OriginBurst>,
    /// Timed cuts between sites.
    pub site_cuts: Vec<SiteCut>,
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

/// Which peer connection slots a Model run holds.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PeerConnections {
    /// Every link connects at once and holds nothing.
    Off,
    /// [`ConnectionConfig::production`] for the spec's workers.
    Production,
    /// Explicit limits.
    Custom(ConnectionConfig),
}

impl Default for ModelSpec {
    /// The default node shape: 2 workers, 16 pages of 1 MiB, 10 s reads.
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
            crypto_queue: crypto_queue(),
            connections: PeerConnections::Production,
            circuits: true,
            waiters_per_flight: flight_limits().waiters_per_flight,
            checks: Vec::new(),
            control: None,
            workload: None,
            origin_bursts: Vec::new(),
            site_cuts: Vec::new(),
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
