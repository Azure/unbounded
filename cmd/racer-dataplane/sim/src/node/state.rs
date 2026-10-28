//! Per-node and per-worker state: the real caches and tables, payload-free.

use std::rc::Rc;
use std::time::{Duration, Instant};

use flow_control::Circuits;
use page_alloc::retention::{Heat, SecondSight};
use page_alloc::{Alignment, PhantomEntries, SegmentClock, Segments};
use racer_core::cache::TickLru;
use racer_core::liveness::{Rank0Watch, WATCH_CAPACITY, WATCH_TTL};
use racer_core::liveness::{SILENCE_CAPACITY, Silence};
use racer_core::read::flight::{FlightLimits, PageFlights};
use racer_core::read::hints::{HINT_CAPACITY, HINT_TTL, SuccessorHints};
use racer_core::read::hints::{NOTICE_CAPACITY, OrphanNotices};
use topology::{MAX_DEGREE, Paths, Phantom, Placement, SharedRoutes};

use super::admission::{Rejections, SimCharge, SimPolicy, SimQuotas};
use super::config::NodeConfig;
use crate::engine::Fixed;
use flow_control::coalesce::flight::state::Published;

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
    /// Shared with in-flight ranking tasks (`world::Task`), which hold it
    /// across yields as production's `rank_async` future does.
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
    /// Generation of the first membership adopted since (re)start: the
    /// start of this process's membership chain (production builds its
    /// chain from the first publication it accepts), so earlier arrivals
    /// are unknown to it (Known issue 25).
    pub lineage_base: Option<u32>,
    pub down: bool,
    pub incarnation: u32,
    /// Speculative hedge slots and bytes shared by the node's workers
    /// (production `Hedges`; zero slots disable hedging).
    pub hedges: std::sync::Arc<flow_control::Hedges>,
    /// Pages this node answered without a copy, and who asked (production
    /// `Coordinator` successor hints, Known issue 21).
    pub hints: SuccessorHints<u64, u32>,
    /// Pages this node fetched from origin as an owner above rank 0, to
    /// offer to rank 0 later (production orphan notices, Known issue 37).
    pub notices: OrphanNotices<(u64, u8), u32, u16>,
    /// Pages whose rank 0 this node, an owner above it, found fetching or
    /// overdue (production `CandidatePolicy::rank0_watch`, Known issue 39).
    pub rank0: Rank0Watch<u64, u32>,
    /// Next hops that left exchanges unanswered (production
    /// `LinkHealth::silence`, Known issues 41 and 43).
    pub silence: Silence<u32, u64>,
    /// The last few owners that shed this node's page requests, with the
    /// page and when (measurement only: `shed_repeats`, Known issue 54).
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
                // Production: half the client request timeout, and the peer
                // attempt timeout for one ping.
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

    /// Per-component approximate bytes, added into `out`.
    pub fn account(&self, out: &mut Components) {
        for w in &self.workers {
            out.placement += w.placement.retained_bytes();
            out.flights += std::mem::size_of::<Flights>() + w.flights.len() * 256;
            out.memory_lru +=
                std::mem::size_of_val(&w.memory) + w.memory.len() * TICK_LRU_ENTRY_BYTES;
            out.quotas += std::mem::size_of::<SimQuotas>() + 512;
        }
        out.paths += std::mem::size_of::<Paths<SharedRoutes>>()
            + self.paths.cached_entries() * ROUTE_ENTRY_BYTES;
        out.links += std::mem::size_of_val(&self.links)
            + self.links.len() * 64
            + self.suspects.capacity() * 4
            + self.hints.len() * 64
            + self.notices.len() * 64
            + self.rank0.len() * 48
            + self.silence.len() * 64;
        out.disk_index += self.disk.entries.len() * SEGMENT_MAPPING_BYTES;
        out.segments += std::mem::size_of::<Segments>()
            + std::mem::size_of::<SegmentClock>()
            + self.disk.segments.count() * 96;
        out.retention += std::mem::size_of::<Retention>() + self.retention.heat.len() * 48;
        out.node_struct += std::mem::size_of::<Self>();
    }
}

/// Per-component memory, summed over nodes.
#[derive(Debug, Default)]
pub struct Components {
    pub placement: usize,
    pub flights: usize,
    pub memory_lru: usize,
    pub quotas: usize,
    pub paths: usize,
    pub links: usize,
    pub disk_index: usize,
    pub segments: usize,
    pub retention: usize,
    pub node_struct: usize,
}

impl Components {
    pub fn list(&self) -> Vec<(&'static str, usize)> {
        vec![
            ("placement", self.placement),
            ("flights", self.flights),
            ("memory_lru", self.memory_lru),
            ("quotas", self.quotas),
            ("paths", self.paths),
            ("link_health", self.links),
            ("disk_index", self.disk_index),
            ("segments", self.segments),
            ("retention", self.retention),
            ("node_struct", self.node_struct),
        ]
    }
}
