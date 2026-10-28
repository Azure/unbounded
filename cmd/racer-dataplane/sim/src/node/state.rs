//! Per-node state (real caches and tables, payload-free), shared membership
//! versions, and admission over production `racer_core::admission::Rules`.

use std::cell::RefCell;
use std::collections::VecDeque;
use std::num::{NonZeroU32, NonZeroUsize};
use std::rc::Rc;
use std::time::{Duration, Instant};

use flow_control::coalesce::flight::state::Published;
use flow_control::{Charge, Circuits, Policy, Quotas, Rejection};
use page_alloc::retention::{Heat, SecondSight};
use page_alloc::{Alignment, PhantomEntries, SegmentClock, Segments};
use racer_control_wire::{CacheId, NodeId};
use racer_core::admission::{Floors, Limits, Rules};
use racer_core::cache::TickLru;
use racer_core::liveness::{Rank0Watch, SILENCE_CAPACITY, Silence, WATCH_CAPACITY, WATCH_TTL};
use racer_core::read::flight::{FlightLimits, PageFlights, Reserve};
use racer_core::read::hints::{
    HINT_CAPACITY, HINT_TTL, NOTICE_CAPACITY, OrphanNotices, SuccessorHints,
};
use racer_core::read::{Error, WorkerMap};
use racer_object_wire::model::{
    CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, StrongEtag, WorkerId,
};
use topology::{MAX_DEGREE, Member, Paths, Placement};

pub use racer_core::admission::ResourceClass;

use crate::config::NodeConfig;
use crate::engine::{Fixed, Nanos, mix};
use crate::phantom::{Phantom, SharedRoutes};

/// Flight results carry the published page length.
pub type Flights = PageFlights<u32, u32, u32, SimCharge, Fixed>;

fn split(len: u32) -> Published<u32, u32> {
    Published::Complete(len)
}

/// Record size on disk: page, AEAD tag and header, rounded to 4 KiB.
pub fn record_bytes(page_bytes: u64) -> u64 {
    (page_bytes + 16 + 4096).div_ceil(4096) * 4096
}

/// Measured per-entry heap bytes (cache track, design doc table).
pub const ROUTE_ENTRY_BYTES: usize = 199;
pub const SEGMENT_MAPPING_BYTES: usize = 121;
pub const TICK_LRU_ENTRY_BYTES: usize = 67;

pub struct Worker {
    /// Shared with in-flight ranking tasks (`world::Task`), which hold it across yields as
    /// production's `rank_async` future does.
    pub placement: Rc<Placement<Phantom>>,
    pub flights: Flights,
    pub memory: TickLru<u64, u32, Fixed>,
    pub quotas: SimQuotas,
}

impl Worker {
    pub fn new(
        config: &NodeConfig,
        phantom: &Phantom,
        rejections: &Rejections,
        now: Instant,
    ) -> Self {
        let limits = FlightLimits {
            entries: config.flights as usize,
            waiters_per_flight: config.waiters_per_flight as usize,
            ..FlightLimits::default()
        };
        Self {
            placement: Rc::new(Placement::with_rankings(
                config.placement_entries as usize,
                phantom.clone(),
            )),
            flights: PageFlights::new(limits, split, now, Fixed::default()),
            memory: TickLru::default(),
            quotas: SimQuotas::new(SimPolicy::new(config, rejections.clone())),
        }
    }
}

/// Disk tier: real segment table and CLOCK, phantom page index, retention.
pub struct Disk {
    pub segments: Rc<Segments>,
    pub clock: SegmentClock,
    pub entries: PhantomEntries<u64, Fixed>,
}

impl Disk {
    pub fn new(disk_pages: u32, pages_per_segment: u32, page_bytes: u64) -> Self {
        let record = record_bytes(page_bytes);
        let per = u64::from(pages_per_segment.max(1));
        let count = (u64::from(disk_pages) / per).max(2) as usize;
        let segment_bytes = record * per;
        let segments = Segments::new(segment_bytes);
        segments
            .configure(
                segment_bytes * count as u64,
                count,
                Alignment::new(4096, 4096, 4096).expect("valid alignment"),
            )
            .expect("valid geometry");
        let segments = Rc::new(segments);
        Self {
            clock: SegmentClock::new(segments.clone()),
            segments,
            entries: PhantomEntries::default(),
        }
    }

    pub fn segment_bytes(&self) -> u64 {
        self.segments.segment_bytes()
    }
}

/// Retention history and heat (volatile; lost on crash).
pub struct Retention {
    pub history: SecondSight<Fixed>,
    pub heat: Heat<u64, Fixed>,
}

impl Retention {
    pub fn new(history_bytes: u32, heat_entries: u32, now: Instant) -> Self {
        Self {
            history: SecondSight::with_hasher(
                history_bytes as usize,
                Duration::from_secs(60),
                now,
                Fixed::default(),
            )
            .expect("valid history"),
            heat: Heat::with_hasher(heat_entries.max(1) as usize, Fixed::default())
                .expect("valid heat"),
        }
    }
}

pub struct Node {
    pub workers: Vec<Worker>,
    /// Shared with in-flight route search tasks.
    pub paths: Rc<Paths<SharedRoutes>>,
    /// Link circuits, as production `LinkHealth` (`src/topology.rs`).
    pub links: Circuits<u32>,
    /// Neighbors with failure records, for the path `blocked` hint.
    pub suspects: Vec<u32>,
    pub disk: Disk,
    pub retention: Retention,
    /// Last adopted membership version.
    pub version: u64,
    /// Generation of the first membership adopted since (re)start: the start of this process's
    /// membership chain (production builds its chain from the first publication it accepts), so
    /// earlier arrivals are unknown to it.
    pub lineage_base: Option<u32>,
    pub down: bool,
    pub incarnation: u32,
    /// Speculative hedge slots and bytes shared by the node's workers (production `Hedges`; zero
    /// slots disable hedging).
    pub hedges: std::sync::Arc<flow_control::Hedges>,
    /// Pages this node answered without a copy, and who asked (production `Coordinator` successor
    /// hints).
    pub hints: SuccessorHints<u64, u32>,
    /// Pages this node fetched from origin as an owner above rank 0, to offer to rank 0 later
    /// (production orphan notices).
    pub notices: OrphanNotices<(u64, u8), u32, u16>,
    /// Pages whose rank 0 this node, an owner above it, found fetching or overdue (production
    /// `CandidatePolicy::rank0_watch`).
    pub rank0: Rank0Watch<u64, u32>,
    /// Next hops that left exchanges unanswered (production `LinkHealth::silence`).
    pub silence: Silence<u32, u64>,
    /// The last few owners that shed this node's page requests, with the page and when (measurement
    /// only: `shed_repeats`).
    pub recent_sheds: Vec<(u32, u64, crate::engine::Nanos)>,
}

/// Bound on [`Node::recent_sheds`].
pub const RECENT_SHEDS: usize = 8;

pub struct Shared {
    pub phantom: Phantom,
    pub routes: SharedRoutes,
    pub rejections: Rejections,
}

impl Node {
    pub fn new(
        config: &NodeConfig,
        workers: u8,
        disk: Disk,
        shared: &Shared,
        disk_pages: u32,
        now: Instant,
    ) -> Self {
        Self {
            workers: (0..workers.max(1))
                .map(|_| Worker::new(config, &shared.phantom, &shared.rejections, now))
                .collect(),
            paths: Rc::new(
                Paths::with_searches(
                    config.path_entries as usize,
                    config.path_bytes as usize,
                    config.active_searches as usize,
                    shared.routes.clone(),
                )
                .with_queue(config.search_queue as usize),
            ),
            links: Circuits::new(MAX_DEGREE, Duration::from_secs(1)),
            suspects: Vec::new(),
            disk,
            retention: Retention::new(config.history_bytes, disk_pages, now),
            version: 1,
            lineage_base: None,
            down: false,
            incarnation: 0,
            hedges: flow_control::Hedges::new(
                config.hedge_slots as usize,
                config.hedge_bytes as usize,
            ),
            hints: SuccessorHints::new(HINT_CAPACITY, HINT_TTL),
            notices: OrphanNotices::new(NOTICE_CAPACITY),
            rank0: Rank0Watch::new(
                WATCH_CAPACITY,
                WATCH_TTL,
                // Production: half the client request timeout.
                Duration::from_nanos(config.read_timeout_ns / 2),
            ),
            silence: Silence::new(
                SILENCE_CAPACITY,
                // Production: half the client request timeout, and the peer attempt timeout for one
                // ping.
                Duration::from_nanos(config.read_timeout_ns / 2),
                Duration::from_nanos(config.attempt_timeout_ns),
            ),
            recent_sheds: Vec::new(),
        }
    }

    /// Volatile state after a restart: everything but the disk.
    pub fn restart(&mut self, config: &NodeConfig, shared: &Shared, disk_pages: u32, now: Instant) {
        let workers = self.workers.len() as u8;
        let disk = std::mem::replace(&mut self.disk, Disk::new(0, 1, 1));
        let incarnation = self.incarnation + 1;
        let version = self.version;
        *self = Self::new(config, workers, disk, shared, disk_pages, now);
        self.incarnation = incarnation;
        self.version = version;
    }

    /// Approximate bytes per [`COMPONENTS`] entry, added into `out`.
    pub fn account(&self, out: &mut [usize; COMPONENTS.len()]) {
        for w in &self.workers {
            out[0] += w.placement.retained_bytes();
            out[1] += std::mem::size_of::<Flights>() + w.flights.len() * 256;
            out[2] += std::mem::size_of_val(&w.memory) + w.memory.len() * TICK_LRU_ENTRY_BYTES;
            out[3] += std::mem::size_of::<SimQuotas>() + 512;
        }
        out[4] += std::mem::size_of::<Paths<SharedRoutes>>()
            + self.paths.cached_entries() * ROUTE_ENTRY_BYTES;
        out[5] += std::mem::size_of_val(&self.links)
            + self.links.len() * 64
            + self.suspects.capacity() * 4
            + self.hints.len() * 64
            + self.notices.len() * 64
            + self.rank0.len() * 48
            + self.silence.len() * 64;
        out[6] += self.disk.entries.len() * SEGMENT_MAPPING_BYTES;
        out[7] += std::mem::size_of::<Segments>()
            + std::mem::size_of::<SegmentClock>()
            + self.disk.segments.count() * 96;
        out[8] += std::mem::size_of::<Retention>() + self.retention.heat.len() * 48;
        out[9] += std::mem::size_of::<Self>();
    }
}

pub const COMPONENTS: [&str; 10] = [
    "placement",
    "flights",
    "memory_lru",
    "quotas",
    "paths",
    "link_health",
    "disk_index",
    "segments",
    "retention",
    "node_struct",
];

/// Interned cache key used for quota fairness (the only simulated cache).
pub const CACHE: u32 = 0;

/// Wire identities for simulated pages and nodes.
pub struct Ids {
    pages_per_object: u64,
    nodes: Vec<NodeId>,
    etag: StrongEtag,
    workers: WorkerMap,
}

impl Ids {
    pub fn new(nodes: u32, workers: u8, pages_per_object: u32) -> Self {
        let map = WorkerMap::new((0..u16::from(workers.max(1))).map(WorkerId).collect())
            .expect("distinct workers");
        Self {
            pages_per_object: u64::from(pages_per_object.max(1)),
            nodes: (0..nodes).map(|n| NodeId(format!("n{n:08}"))).collect(),
            etag: StrongEtag::parse(b"\"v1\"").expect("valid etag"),
            workers: map,
        }
    }

    pub fn node(&self, node: u32) -> &NodeId {
        &self.nodes[node as usize]
    }

    pub fn object(&self, page: u64) -> ObjectId {
        let mut key = [0u8; 32];
        key[..8].copy_from_slice(&(page / self.pages_per_object).to_be_bytes());
        ObjectId {
            cache: CacheId("sim".to_owned()),
            key: CacheKey(key),
        }
    }

    pub fn number(&self, page: u64) -> PageNumber {
        PageNumber(page % self.pages_per_object)
    }

    pub fn page(&self, page: u64) -> PageId {
        PageId {
            version: ObjectVersion {
                object: self.object(page),
                etag: self.etag.clone(),
            },
            number: self.number(page),
        }
    }

    /// Inverse of [`Self::page`].
    pub fn key(&self, page: &PageId) -> u64 {
        let object = u64::from_be_bytes(
            page.version.object.key.0[..8]
                .try_into()
                .expect("eight bytes"),
        );
        object * self.pages_per_object + page.number.0
    }

    /// Local worker for `page` (`WorkerMap::select`, production dispatch).
    pub fn worker(&self, page: u64) -> u8 {
        let object = self.object(page);
        self.workers
            .select(&object, self.number(page).0)
            .expect("valid object")
            .0 as u8
    }

    pub fn approximate_bytes(&self) -> usize {
        self.nodes.capacity() * std::mem::size_of::<NodeId>()
            + self.nodes.iter().map(|n| n.0.capacity()).sum::<usize>()
    }
}

/// A simulated member: node id `n` has the 4-byte big-endian id of `n`, so positions sort in node
/// order.
#[derive(Clone, Copy, Debug)]
pub struct SimMember {
    pub node: u32,
    id: [u8; 4],
}

impl SimMember {
    pub fn new(node: u32) -> Self {
        Self {
            node,
            id: node.to_be_bytes(),
        }
    }
}

impl Member for SimMember {
    const DOMAIN: &'static str = "racer-sim";

    fn id(&self) -> &[u8] {
        &self.id
    }

    fn weight(&self) -> NonZeroU32 {
        NonZeroU32::MIN
    }
}

pub type Members = topology::Membership<SimMember>;

pub struct Version {
    pub number: u64,
    pub published: Nanos,
    pub members: Rc<Members>,
}

pub struct Versions {
    live: Vec<bool>,
    history: VecDeque<Version>,
    dirty: bool,
    lag: Nanos,
    retained: u64,
    /// Combined mode: the version each node's control client has applied (0 before its first
    /// snapshot).
    external: Option<Vec<u64>>,
    /// Combined mode: members of the latest version whose client has applied nothing, so they
    /// refuse peer links.
    unready: u32,
    /// Combined mode: per node, the generation of the first version its control client applied
    /// since (re)start, where production's membership chain begins.
    chain_base: Vec<Option<u32>>,
}

impl Versions {
    /// Version 1, published at time 0, with every node live.
    pub fn new(nodes: u32, lag: Nanos, retained: u64) -> Self {
        let members = Members::new((0..nodes).map(SimMember::new).collect()).expect("valid ids");
        let mut history = VecDeque::new();
        history.push_back(Version {
            number: 1,
            published: 0,
            members: Rc::new(members),
        });
        Self {
            live: vec![true; nodes as usize],
            history,
            dirty: false,
            lag,
            retained,
            external: None,
            unready: 0,
            chain_base: Vec::new(),
        }
    }

    /// Combined mode: no version until the control plane commits one, and every node adopts only
    /// what its control client applied ([`Self::commit`], [`Self::apply`]).
    pub fn external(nodes: u32, retained: u64) -> Self {
        Self {
            live: vec![false; nodes as usize],
            history: VecDeque::new(),
            dirty: false,
            lag: 0,
            retained,
            external: Some(vec![0; nodes as usize]),
            unready: 0,
            chain_base: vec![None; nodes as usize],
        }
    }

    /// Combined mode: record the committed membership `number` with `members` (node ids).
    pub fn commit(&mut self, number: u64, mut members: Vec<u32>, now: Nanos) -> bool {
        if self.history.back().is_some_and(|v| v.number >= number) {
            return false;
        }
        members.sort_unstable();
        members.dedup();
        members.retain(|n| (*n as usize) < self.live.len());
        self.live.iter_mut().for_each(|l| *l = false);
        for n in &members {
            self.live[*n as usize] = true;
        }
        self.unready = members.iter().filter(|n| !self.enrolled(**n)).count() as u32;
        let list: Vec<SimMember> = members.into_iter().map(SimMember::new).collect();
        let next = match self.history.back() {
            Some(prev) => Members::new_with_predecessor(list, &prev.members),
            None => Members::new(list),
        }
        .expect("valid ids");
        self.history.push_back(Version {
            number,
            published: now,
            members: Rc::new(next),
        });
        true
    }

    /// Combined mode: `node`'s control client applied `number` (0 after a crash). Versions never
    /// committed here are ignored.
    pub fn apply(&mut self, node: u32, number: u64) {
        if number != 0 && self.get(number).is_none() {
            return;
        }
        let generation = self.get(number).map(|v| v.members.generation());
        if let Some(base) = self.chain_base.get_mut(node as usize)
            && (number == 0 || base.is_none())
        {
            *base = generation;
        }
        let live = self.is_live(node);
        if let Some(slot) = self
            .external
            .as_mut()
            .and_then(|e| e.get_mut(node as usize))
        {
            if live && (*slot == 0) != (number == 0) {
                if number == 0 {
                    self.unready += 1;
                } else {
                    self.unready -= 1;
                }
            }
            *slot = number;
        }
    }

    /// Combined mode: where `node`'s membership chain begins (see `chain_base`); `None` in
    /// standalone mode or before it applied any.
    pub fn chain_base(&self, node: u32) -> Option<u32> {
        self.chain_base.get(node as usize).copied().flatten()
    }

    /// Combined mode: members of the latest version that cannot serve yet (their client has applied
    /// nothing: enrolling, or restarted).
    pub fn unready_members(&self) -> u32 {
        self.unready
    }

    /// Combined mode: drop versions no live node can resolve any more (older than the oldest
    /// applied version minus `retained`), keeping the latest.
    pub fn prune(&mut self, down: impl Fn(u32) -> bool) -> bool {
        let Some(external) = &self.external else {
            return false;
        };
        let Some(latest) = self.history.back().map(|v| v.number) else {
            return false;
        };
        let oldest = external
            .iter()
            .enumerate()
            .filter(|(n, v)| **v > 0 && !down(*n as u32))
            .map(|(_, v)| *v)
            .min()
            .unwrap_or(latest);
        let mut dropped = false;
        while self.history.len() > 1
            && self
                .history
                .get(self.retained as usize + 1)
                .is_some_and(|v| v.number <= oldest)
        {
            self.history.pop_front();
            dropped = true;
        }
        dropped
    }

    /// Whether `node` has a membership to work with: always in standalone mode; in combined mode
    /// once its control client applied a version.
    pub fn enrolled(&self, node: u32) -> bool {
        match &self.external {
            None => true,
            Some(e) => e.get(node as usize).is_some_and(|v| *v > 0),
        }
    }

    pub fn is_live(&self, node: u32) -> bool {
        self.live.get(node as usize).copied().unwrap_or(false)
    }

    /// Mark a change for the next publication. Returns whether it changed.
    pub fn set(&mut self, node: u32, live: bool) -> bool {
        let Some(slot) = self.live.get_mut(node as usize) else {
            return false;
        };
        if *slot == live {
            return false;
        }
        *slot = live;
        self.dirty = true;
        true
    }

    pub fn pending(&self) -> bool {
        self.dirty
    }

    /// Publish pending changes as a new version.
    pub fn publish(&mut self, now: Nanos) -> bool {
        if !self.dirty {
            return false;
        }
        self.dirty = false;
        let latest = self.latest();
        let members: Vec<SimMember> = (0..self.live.len() as u32)
            .filter(|n| self.live[*n as usize])
            .map(SimMember::new)
            .collect();
        let next = Members::new_with_predecessor(members, &latest.members).expect("valid ids");
        let number = latest.number + 1;
        self.history.push_back(Version {
            number,
            published: now,
            members: Rc::new(next),
        });
        // Nodes lag at most one batch, so they adopt at least `number - 1`; they resolve `retained`
        // versions older than that.
        let keep = self.retained as usize + 2;
        let mut dropped = false;
        while self.history.len() > keep {
            self.history.pop_front();
            dropped = true;
        }
        dropped
    }

    pub fn latest(&self) -> &Version {
        self.history.back().expect("at least one version")
    }

    /// Latest version number, 0 before any (combined mode).
    pub fn latest_number(&self) -> u64 {
        self.history.back().map_or(0, |v| v.number)
    }

    pub fn get(&self, number: u64) -> Option<&Version> {
        // Numbers are increasing but may skip in combined mode.
        let i = self
            .history
            .binary_search_by_key(&number, |v| v.number)
            .ok()?;
        self.history.get(i)
    }

    /// Per-node adoption lag for `version` (version 1 has none).
    pub fn lag(&self, node: u32, version: u64) -> Nanos {
        if version <= 1 || self.lag == 0 {
            return 0;
        }
        mix((u64::from(node) << 32) ^ version) % (self.lag + 1)
    }

    /// Newest version `node` has adopted at `now`.
    pub fn adopted(&self, node: u32, now: Nanos) -> &Version {
        if let Some(e) = &self.external {
            let applied = e.get(node as usize).copied().unwrap_or(0);
            return self
                .get(applied)
                .unwrap_or_else(|| self.history.front().expect("nonempty"));
        }
        self.history
            .iter()
            .rev()
            .find(|v| v.published + self.lag(node, v.number) <= now)
            .unwrap_or_else(|| self.history.front().expect("nonempty"))
    }

    /// Whether `node` can resolve `version` at `now` (its adopted version or one of the `retained`
    /// before it).
    pub fn known(&self, node: u32, version: u64, now: Nanos) -> bool {
        if !self.enrolled(node) {
            return false;
        }
        let adopted = self.adopted(node, now).number;
        let retained = match &self.external {
            // Count retained versions, not numbers (numbers may skip).
            Some(_) => {
                let lo = self.history.partition_point(|v| v.number < version);
                let hi = self.history.partition_point(|v| v.number < adopted);
                return version <= adopted
                    && (hi - lo) as u64 <= self.retained
                    && self.get(version).is_some();
            }
            None => self.retained,
        };
        version <= adopted && adopted - version <= retained && self.get(version).is_some()
    }

    /// Membership identities to keep in shared placement memos.
    pub fn identities(&self) -> Vec<[u8; 32]> {
        self.history.iter().map(|v| v.members.identity()).collect()
    }

    /// Graph identities to keep in shared route memos.
    pub fn topology_identities(&self) -> Vec<[u8; 32]> {
        self.history
            .iter()
            .map(|v| v.members.topology_identity())
            .collect()
    }

    pub fn approximate_bytes(&self) -> usize {
        self.live.capacity()
            + self.external.as_ref().map_or(0, |e| e.capacity() * 8)
            + self
                .history
                .iter()
                .map(|v| v.members.retained_bytes())
                .sum::<usize>()
    }
}

/// Rejection counts per class, plus key-table rejections at the end.
pub type Rejections = Rc<RefCell<[u64; ResourceClass::ALL.len() + 1]>>;

pub struct SimPolicy {
    rules: Rules,
    rejections: Rejections,
}

/// Simulated page size; production pages are 16 MiB (`PAGE_BYTES`).
pub const PAGE_BYTES: usize = 1 << 20;

/// Production `store::MAX_HEADER_BYTES` (`src/store.rs`), the per-record overhead in the ciphertext
/// floor.
pub const MAX_HEADER_BYTES: usize = 16384 + 4096;

fn nonzero(value: u64) -> NonZeroUsize {
    NonZeroUsize::new(usize::try_from(value).unwrap_or(usize::MAX)).unwrap_or(NonZeroUsize::MIN)
}

/// Production per-worker limits with the read-path dimensions the simulator configures. One
/// simulated node is one production I/O worker.
pub fn limits(config: &NodeConfig) -> Limits {
    Limits {
        plaintext_bytes: nonzero(config.plaintext_bytes),
        ciphertext_bytes: nonzero(config.ciphertext_bytes),
        dirty_bytes: nonzero(config.dirty_bytes),
        flights: nonzero(config.flights.into()),
        waiters_per_flight: nonzero(config.waiters_per_flight.into()),
        relay_transfers: nonzero(config.relay_transfers.into()),
        cached_paths: nonzero(config.path_entries.into()),
        path_cache_bytes: nonzero(config.path_bytes),
        active_path_searches: nonzero(config.active_searches.into()),
        retained_snapshots: nonzero(config.retained_versions),
        ..Limits::default()
    }
}

impl SimPolicy {
    pub fn new(config: &NodeConfig, rejections: Rejections) -> Self {
        Self {
            rules: Rules::new(
                limits(config),
                Floors::for_page(PAGE_BYTES, MAX_HEADER_BYTES),
            ),
            rejections,
        }
    }
}

impl Policy for SimPolicy {
    type Class = ResourceClass;
    type Key = u32;

    fn limit(&self, class: ResourceClass) -> usize {
        self.rules.limit(class)
    }

    fn floor(&self, class: ResourceClass) -> usize {
        self.rules.floor(class)
    }

    fn progress(&self, class: ResourceClass) -> usize {
        self.rules.progress(class)
    }

    fn weighted(class: ResourceClass) -> bool {
        Rules::weighted(class)
    }

    fn max_keys(&self) -> usize {
        self.rules.max_keys()
    }

    fn wakes(class: ResourceClass) -> bool {
        Rules::wakes(class)
    }

    fn allows_stopped(class: ResourceClass) -> bool {
        Rules::allows_stopped(class)
    }

    fn covers(class: ResourceClass) -> bool {
        Rules::covers(class)
    }

    fn rejected(&self, rejection: Rejection<ResourceClass>) {
        let index = match rejection {
            Rejection::Resource { class, .. } => class as usize,
            Rejection::Keys { .. } => ResourceClass::ALL.len(),
        };
        self.rejections.borrow_mut()[index] += 1;
    }
}

pub type SimQuotas = Quotas<SimPolicy>;
pub type SimCharge = Charge<SimPolicy>;

/// Map a flow-control failure onto the read-path error.
pub fn read_error(error: flow_control::Error) -> Error {
    match error {
        flow_control::Error::Overloaded => Error::Overloaded,
        flow_control::Error::InvalidInput => Error::InvalidRequest,
        flow_control::Error::Unavailable | flow_control::Error::Io => Error::Unavailable,
    }
}

/// Reserve `amount` of `class` for the simulated cache.
pub fn reserve(
    quotas: &SimQuotas,
    class: ResourceClass,
    amount: usize,
) -> Result<SimCharge, Error> {
    quotas
        .reserve(Some(&CACHE), class, amount)
        .map_err(read_error)
}

/// A flight table reservation (`PageFlights::join`), held by the waiter or flight it admits as
/// production `Flights` holds it.
pub fn flight_charge(quotas: &SimQuotas, kind: Reserve) -> Result<SimCharge, Error> {
    let class = match kind {
        Reserve::Waiter => ResourceClass::Waiter,
        Reserve::Flight => ResourceClass::Flight,
    };
    reserve(quotas, class, 1)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn round_trips_and_spreads_workers() {
        let ids = Ids::new(4, 2, 16);
        for page in [0, 15, 16, 12_345] {
            assert_eq!(ids.key(&ids.page(page)), page);
        }
        assert_eq!(ids.page(17).number, PageNumber(1));
        assert!(ids.node(0).0 < ids.node(3).0);
        let workers: std::collections::BTreeSet<u8> = (0..64).map(|p| ids.worker(p)).collect();
        assert_eq!(workers.into_iter().collect::<Vec<_>>(), [0, 1]);
        assert_eq!(ids.worker(5), ids.worker(5));
    }

    #[test]
    fn external_versions_follow_the_control_plane() {
        let mut v = Versions::external(6, 1);
        assert_eq!(v.latest_number(), 0);
        assert!(!v.enrolled(0));
        assert!(!v.known(0, 1, 0));
        assert!(v.commit(3, vec![0, 1, 2, 9], 10));
        assert!(!v.commit(3, vec![0], 11), "not newer");
        assert_eq!(
            v.latest().members.members().len(),
            3,
            "out of range dropped"
        );
        assert!(v.is_live(2) && !v.is_live(3));
        v.apply(0, 3);
        v.apply(1, 7); // never committed: ignored
        assert!(v.enrolled(0) && !v.enrolled(1));
        assert_eq!(v.adopted(0, 0).number, 3);
        assert!(v.commit(5, vec![0, 1, 2, 3], 20));
        assert!(v.commit(8, vec![0, 1, 3], 30));
        v.apply(1, 8);
        // Node 0 is at 3: it resolves 3 only; node 1 resolves 8 and 5.
        assert!(v.known(0, 3, 0) && !v.known(0, 5, 0));
        assert!(v.known(1, 5, 0) && v.known(1, 8, 0));
        assert!(!v.known(1, 3, 0), "older than retained");
        assert!(!v.prune(|_| false), "node 0 still needs 3");
        v.apply(0, 8);
        assert!(v.prune(|_| false));
        assert!(v.get(3).is_none());
        assert!(v.get(5).is_some(), "retained before the oldest applied");
        // A crashed node does not pin old versions.
        v.apply(2, 5);
        assert!(!v.prune(|n| n == 2));
        assert!(v.get(5).is_some() && v.get(8).is_some());
        v.apply(0, 0);
        assert!(!v.enrolled(0));
    }

    #[test]
    fn publish_adopt_and_resolve() {
        let mut v = Versions::new(8, 10, 1);
        assert!(!v.publish(5), "nothing pending");
        assert!(v.set(3, false));
        assert!(!v.set(3, false));
        assert!(!v.set(99, false));
        assert!(v.pending());
        assert!(!v.publish(100));
        assert_eq!(v.latest().number, 2);
        assert_eq!(v.latest().members.members().len(), 7);
        assert_eq!(v.latest().members.position(&3u32.to_be_bytes()), None);
        assert_eq!(v.latest().members.position(&4u32.to_be_bytes()), Some(3));
        // Adoption is lagged per node, never before publication.
        for node in 0..8 {
            assert_eq!(v.adopted(node, 99).number, 1);
            assert_eq!(v.adopted(node, 110).number, 2);
            let at = 100 + v.lag(node, 2);
            assert_eq!(v.adopted(node, at).number, 2);
            assert!(v.known(node, 1, 110));
            assert!(!v.known(node, 2, 99), "not adopted yet");
        }
        // History is bounded; old versions become unknown.
        for (i, (node, live)) in [(3u32, true), (4, false), (5, false)]
            .into_iter()
            .enumerate()
        {
            assert!(v.set(node, live));
            v.publish(200 + 100 * i as u64);
        }
        assert_eq!(v.latest().number, 5);
        assert!(v.get(1).is_none());
        assert!(v.get(2).is_none());
        assert!(v.get(3).is_some());
        assert!(!v.known(0, 3, 1000), "older than retained");
        assert!(v.known(0, 4, 1000));
        assert_eq!(v.identities().len(), 3);
        assert_eq!(v.topology_identities().len(), 3);
        assert!(v.approximate_bytes() > 0);
    }

    #[test]
    fn limits_rejections_and_flight_charges() {
        let rejections = Rejections::default();
        let config = NodeConfig {
            flights: 1,
            waiters_per_flight: 2,
            ..NodeConfig::default()
        };
        let quotas = SimQuotas::new(SimPolicy::new(&config, rejections.clone()));
        assert_eq!(quotas.limit(ResourceClass::Waiter), 2);
        assert_eq!(quotas.limit(ResourceClass::Relay), 16);
        let wa = flight_charge(&quotas, Reserve::Waiter).unwrap();
        let fa = flight_charge(&quotas, Reserve::Flight).unwrap();
        let wb = flight_charge(&quotas, Reserve::Waiter).unwrap();
        assert!(matches!(
            flight_charge(&quotas, Reserve::Flight),
            Err(Error::Overloaded)
        ));
        assert!(matches!(
            flight_charge(&quotas, Reserve::Waiter),
            Err(Error::Overloaded)
        ));
        assert_eq!(quotas.used(ResourceClass::Waiter), 2);
        drop((wa, fa, wb));
        assert_eq!(quotas.used(ResourceClass::Waiter), 0);
        assert_eq!(quotas.used(ResourceClass::Flight), 0);
        // Policy sees every failed attempt, including the reclamation retry.
        let counts = *rejections.borrow();
        assert!(counts[ResourceClass::Flight as usize] >= 1);
        assert!(counts[ResourceClass::Waiter as usize] >= 1);
        assert_eq!(counts[ResourceClass::Plaintext as usize], 0);
        assert!(reserve(&quotas, ResourceClass::Relay, 17).is_err());
        assert_eq!(ResourceClass::Relay.name(), "relay");
        // Production rules: unconfigured classes keep production defaults.
        assert_eq!(quotas.limit(ResourceClass::IngressConnection), 45);
        assert_eq!(quotas.policy().max_keys(), 4096);
        assert_eq!(
            quotas.policy().floor(ResourceClass::Ciphertext),
            PAGE_BYTES + (64 << 10)
        );
        assert_eq!(read_error(flow_control::Error::Io), Error::Unavailable);
    }
}
