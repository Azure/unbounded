//! Per-node and per-worker state: the real caches and tables, payload-free.

use std::collections::HashMap;
use std::rc::Rc;
use std::time::{Duration, Instant};

use flow_control::Circuits;
use page_alloc::retention::{Heat, SecondSight};
use page_alloc::{Alignment, PhantomEntries, SegmentClock, Segments};
use racer_core::cache::lru::TickLru;
use racer_core::read::flight::{FlightLimits, PageFlights};
use topology::{MAX_DEGREE, Paths, Phantom, Placement, SharedRoutes};

use super::admission::{PageCharges, Rejections, SimPolicy, SimQuotas};
use super::config::NodeConfig;
use crate::scenario::Fixed;
use flow_control::coalesce::flight::state::Published;

/// Flight results carry the published page length.
pub type Flights = PageFlights<u32, u32, u32, Fixed>;

fn split(len: u32) -> Published<u32, u32> {
    Published::Complete(len)
}

/// Record size on disk: page, AEAD tag and header, rounded to 4 KiB.
pub fn record_bytes(page_bytes: u64) -> u64 {
    (page_bytes + 16 + 4096).div_ceil(4096) * 4096
}

/// Measured per-entry heap bytes (cache track, design doc table).
pub const PLACEMENT_ENTRY_BYTES: usize = 99;
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
    pub charges: HashMap<u64, PageCharges, Fixed>,
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
            flights: PageFlights::with_hasher(limits, split, now, Fixed::default()),
            memory: TickLru::default(),
            quotas: SimQuotas::new(SimPolicy::new(config, rejections.clone())),
            charges: HashMap::default(),
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
    pub down: bool,
    pub incarnation: u32,
    /// Speculative hedge slots and bytes shared by the node's workers
    /// (production `Hedges`; zero slots disable hedging).
    pub hedges: std::sync::Arc<flow_control::Hedges>,
}

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
            paths: Rc::new(Paths::with_searches(
                config.path_entries as usize,
                config.path_bytes as usize,
                config.active_searches as usize,
                shared.routes.clone(),
            )),
            links: Circuits::new(MAX_DEGREE, Duration::from_secs(1)),
            suspects: Vec::new(),
            disk,
            retention: Retention::new(config.history_bytes, disk_pages, now),
            version: 1,
            down: false,
            incarnation: 0,
            hedges: flow_control::Hedges::new(
                config.hedge_slots as usize,
                config.hedge_bytes as usize,
            ),
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
            out.quotas += std::mem::size_of::<SimQuotas>()
                + 512
                + w.charges.capacity() * (8 + std::mem::size_of::<PageCharges>());
        }
        out.paths += std::mem::size_of::<Paths<SharedRoutes>>()
            + self.paths.cached_entries() * ROUTE_ENTRY_BYTES;
        out.links += std::mem::size_of_val(&self.links)
            + self.links.len() * 64
            + self.suspects.capacity() * 4;
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
