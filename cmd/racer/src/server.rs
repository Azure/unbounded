//! Maps ublk requests onto allocator pages.
//!
//! The runtime sizes each device so one request is at most one page and never straddles
//! two; the allocator owns placement, versioning and integrity. What is left is address
//! arithmetic and two rules: a hole reads as zeroes (block layer), and a 4 MiB page is
//! written whole or not at all (out-of-place placement). A node exports one block device
//! per configured device and one *fabric* device per universe for its peers; both share
//! the per-core workers and zero-copy path and differ only in the LBA, an offset into a
//! concatenation of extents or the frame itself.

use std::time::{Duration, Instant};

use crate::alloc::{self, Allocator, GlobalAddr, Pressure, Status};
use crate::cache::{self, Cache};
use crate::config::{self, Config, Device};
use crate::fabric::{
    self, Class, Cmd, Footer, GroupIx, Link, PageRef, Put, Source, To, Want, status,
};
use crate::heal::{self, Heal};
use crate::layout;
use crate::metrics;
use crate::paxos::{self, Page, Paxos, Register, Sink};
use crate::runtime::{
    self, Buf, Cfg, Configurator, Errno, Export, Handler, Op, PoolBuf, Request, sleep,
};

pub struct Server;
pub static SERVER: Server = Server;

/// Tag bit separating the two kinds of device key the runtime sees.
///
/// Consumer keys are configured device ids, fabric keys are universe ids; both are 32-bit
/// and independently assigned. This bit is the whole demultiplex in [`Handler::handle`].
pub(crate) const FABRIC_TAG: u64 = 1 << 32;

/// The runtime device key of a universe's fabric device.
pub(crate) fn fabric_key(universe: u32) -> u64 {
    FABRIC_TAG | universe as u64
}

/// One published configuration: the allocator plus the devices it is exported through.
pub struct Dataplane {
    /// What every core reads. Reached from a worker through [`config`], and from anything
    /// already holding the plane through [`Dataplane::config`]; either way a reload cannot
    /// retire it while a borrow is out, because the borrow is a guard on this very value.
    config: Config,
    /// This node's cohort in each universe, derived from the catalogs above. A projection
    /// rather than a second source: it is here so that deriving it is a cost of publishing
    /// a configuration and not of reading one.
    roster: cache::Roster,
    paxos: &'static Paxos,
    cache: &'static Cache,
    heal: &'static Heal,
    /// Universe id and its fabric device, in config order.
    universes: Vec<(u32, Export)>,
    /// Device id and the block device it is exported as, in config order.
    devices: Vec<(u32, Export)>,
}

/// The configuration this worker is running under, pinned until the guard is dropped.
///
/// For the subsystems, which are leaked and reached through `&'static` and so are never
/// handed the plane. A hop closure runs on the destination core long after the caller's
/// borrow was taken, so it re-reads here rather than carrying one across; it sees whatever
/// generation that core has cut over to, which is what a second read here would give
/// anyway. Panics off a worker: the control thread holds the `Config` it is installing.
pub(crate) fn config() -> Cfg<Config> {
    runtime::config::<Server>().map(|d| &d.config)
}

/// This node's cohort in each universe, pinned until the guard is dropped.
///
/// Derived from the same configuration and guarded by the same count, so the cohort a
/// placement decision reads always belongs to the catalog it was computed from.
pub(crate) fn roster() -> Cfg<cache::Roster> {
    runtime::config::<Server>().map(|d| &d.roster)
}

impl Drop for Dataplane {
    fn drop(&mut self) {
        self.paxos.retire_links();
    }
}

impl Dataplane {
    pub(crate) fn alloc(&self) -> &'static Allocator {
        self.paxos.alloc()
    }

    /// What this configuration says. Borrowed from the plane, so it cannot outlive it.
    pub(crate) fn config(&self) -> &Config {
        &self.config
    }

    /// Device id and the block device it is exported as, in config order.
    pub fn devices(&self) -> Vec<(u32, std::path::PathBuf)> {
        self.devices
            .iter()
            .map(|(id, v)| (*id, v.path().to_path_buf()))
            .collect()
    }

    /// Universe id and the device that universe's peers issue fabric frames against.
    ///
    /// Published through nvmet to that universe's members only: the partition is the set
    /// of nodes holding the namespace. Holds no bytes of its own.
    pub fn fabrics(&self) -> Vec<(u32, std::path::PathBuf)> {
        self.universes
            .iter()
            .map(|(id, v)| (*id, v.path().to_path_buf()))
            .collect()
    }

    /// Metadata blocks whose two copies both failed at startup and were taken out of use.
    pub fn quarantined(&self) -> usize {
        self.alloc().quarantined
    }

    /// What must hold of this node's internal state whatever has been done to it. The
    /// simulator samples this after every action; nothing else calls it.
    #[cfg(feature = "sim")]
    pub fn invariants(&self) -> Result<(), String> {
        self.alloc().invariants()
    }

    /// Pages part-way through arriving from the fabric.
    #[cfg(feature = "sim")]
    pub fn assemblies(&self) -> usize {
        self.alloc().assemblies()
    }
}

/// The consensus layer, and through it the allocator, for one node.
///
/// Built once and reused by every later configuration: allocator geometry is fixed at
/// format time and its shards are sized for the worker count, so a reload only swaps the
/// config it reads. A value rather than statics because the simulator holds one per node.
#[derive(Default)]
pub struct Node {
    paxos: std::sync::OnceLock<&'static Paxos>,
    heal: std::sync::OnceLock<&'static Heal>,
}

impl Node {
    pub fn new() -> Node {
        Node::default()
    }

    /// Build a configuration: open the store, install `cfg`, and attach a ublk device per
    /// configured device plus one fabric device per universe. A re-declared device is
    /// kept, one that is not is torn down, and a live registration is never disturbed;
    /// re-placing an extent touches no device, since the new config carries it.
    pub fn attach(&self, c: &Configurator, cfg: Config) -> std::io::Result<Dataplane> {
        let store = cfg.node.store.clone();
        let limit =
            runtime::Limiter::new(cfg.node.store_max_iops, cfg.node.store_max_bytes_per_sec);
        let disk = c.disk(&store, None, Some(limit))?;
        let cores = c.cores();
        // Declare everything the new config asks for before publishing it: a failure
        // below is a rejected config, so the allocator must still read the running one.
        let mut devices = Vec::new();
        for dev in cfg.devices() {
            let e = c.device(dev.id as u64, dev.id, dev.bytes(), dev.class())?;
            devices.push((dev.id, e));
        }
        // Sparse: a fabric device is an address space, not storage. One per universe, and
        // the minor is the universe's, not the routing key: peers open it by path.
        let mut universes = Vec::new();
        for u in cfg.universes() {
            let e = c.fabric(fabric_key(u.id), u.fabric_device_id, fabric::DEVICE_SIZE)?;
            universes.push((u.id, e));
        }
        let mut links = Vec::new();
        for (universe, p) in cfg.peers() {
            links.push(Link::open(c, universe, p)?);
        }

        // Point of no return: nothing past here can fail on a reload.
        let paxos = match self.paxos.get() {
            Some(&p) => p,
            None => {
                let alloc = alloc::open(&store, disk, &cfg, cores)?;
                // One metric row per worker; the worker count is settled here.
                metrics::init(cores);
                // Leaked like the allocator: a hop closure must be `'static`.
                let cache = cache::open(alloc, &cfg, cores);
                // The allocator loans free 4 MiB slots back through this from inside a
                // reservation. Installed before any worker runs.
                alloc.attach(cache);
                let p = paxos::open(alloc, cache, cores);
                let _ = self.paxos.set(p);
                let _ = self.heal.set(heal::open(p, cores));
                p
            }
        };
        paxos.install_links(links);
        let heal = *self.heal.get().expect("set beside paxos");
        Ok(Dataplane {
            roster: cache::Roster::of(&cfg),
            config: cfg,
            paxos,
            cache: paxos.cache(),
            heal,
            universes,
            devices,
        })
    }
}

/// A plane with no devices, fabric or links: everything a test needs to drive one
/// subsystem under a real configuration, and nothing that needs a kernel device or a peer.
///
/// The cache is built but deliberately not attached to the allocator, so no slot is lent
/// away behind a test that is counting them.
#[cfg(test)]
pub(crate) fn bare_plane(c: &Configurator, cfg: Config) -> std::io::Result<Dataplane> {
    let store = cfg.node.store.clone();
    let disk = c.disk(&store, None, None)?;
    let cores = c.cores();
    let alloc = alloc::open(&store, disk, &cfg, cores)?;
    metrics::init(cores);
    let cache = cache::open(alloc, &cfg, cores);
    let paxos = paxos::open(alloc, cache, cores);
    Ok(Dataplane {
        roster: cache::Roster::of(&cfg),
        config: cfg,
        paxos,
        cache,
        heal: heal::open(paxos, cores),
        universes: Vec::new(),
        devices: Vec::new(),
    })
}

/// The state one worker owns outright: shards, cache slots and consensus rows that no
/// other core may touch. Reached only through `runtime::with_core`, and never swapped, so
/// a reload replaces what a core reads and not what it owns.
///
/// Empty for now; the subsystems move their per-core rows in here.
pub struct CoreState {}

impl Handler for Server {
    type Config = Dataplane;
    type CoreState = CoreState;

    fn core_state(&'static self, _cfg: &Dataplane, cores: usize) -> Vec<CoreState> {
        (0..cores).map(|_| CoreState {}).collect()
    }

    async fn handle(&'static self, cfg: Cfg<Dataplane>, req: Request) -> Result<(), Errno> {
        if req.dev & FABRIC_TAG != 0 {
            // Boxed: each worker preallocates one slot per tag sized for the larger of a
            // fabric and a consumer future, and this one is much larger.
            let universe = req.dev as u32;
            return Box::pin(dispatch(&cfg, universe, req)).await;
        }
        serve(&cfg, req).await.map_err(Status::errno)
    }

    fn tick(&'static self, cfg: Cfg<Dataplane>, now: Instant) {
        cfg.alloc().tick(now);
        // Sketch decay: by elapsed time, not per tick, since a parked worker takes none.
        cfg.cache.tick(now);
        // `tick` is not async, so this only starts a sweep; it runs detached on this core.
        cfg.heal.tick(now);
        sample(&cfg);
    }
}

/// Publish this core's counters. Each worker owns one row and writes only its own, so a
/// scrape is a sum, never a lock. Core 0 alone reports node-wide values, which the sum
/// would otherwise multiply by the worker count.
fn sample(d: &Dataplane) {
    let core = runtime::core();
    let a = d.alloc();
    let p = d.paxos.local_stats();
    let h = d.heal.local_stats();
    let c = d.cache.local_stats();
    let [small, huge] = a.capacity();
    let mut s = metrics::Sample {
        paxos_accept_ok: p.accept_ok,
        paxos_accept_rejected: p.accept_rejected,
        paxos_one_shot: p.one_shot,
        paxos_guard_conflicts: p.guard_conflicts,
        paxos_prepares: p.prepares,
        paxos_term_bumps: p.term_bumps,
        paxos_lww_retries: p.lww_retries,
        paxos_repairs: p.repairs,
        paxos_read_matched: p.read_matched,
        paxos_read_remote_match: p.read_remote_match,
        paxos_read_failed: p.read_failed,
        paxos_learn_stale: p.learn_stale,
        paxos_seals: p.seals,
        paxos_groups_unavailable: p.groups_unavailable,
        paxos_gateway_retries: p.gateway_retries,
        paxos_zones_unavailable: p.zones_unavailable,
        paxos_warms_sent: p.warms_sent,
        paxos_warms_taken: p.warms_taken,
        paxos_warms_dropped: p.warms_dropped,
        heal_sweeps: h.sweeps,
        heal_buckets_diff: h.buckets_diff,
        heal_repairs: h.repairs,
        heal_failed: h.failed,
        heal_oversized: h.oversized,
        heal_dropped: h.dropped,
        cache_hits_small: c.per[0].hits,
        cache_hits_huge: c.per[1].hits,
        cache_misses_small: c.per[0].misses,
        cache_misses_huge: c.per[1].misses,
        cache_served_small: c.per[0].served,
        cache_served_huge: c.per[1].served,
        cache_admits_small: c.per[0].admits,
        cache_admits_huge: c.per[1].admits,
        cache_evictions_small: c.per[0].evictions,
        cache_evictions_huge: c.per[1].evictions,
        cache_dropped_small: c.per[0].dropped,
        cache_dropped_huge: c.per[1].dropped,
        cache_stale_small: c.per[0].stale,
        cache_stale_huge: c.per[1].stale,
        cache_shed_small: c.per[0].shed,
        cache_shed_huge: c.per[1].shed,
        cache_reject_policy_small: c.per[0].rejected_policy,
        cache_reject_policy_huge: c.per[1].rejected_policy,
        cache_reject_victim_small: c.per[0].rejected_victim,
        cache_reject_victim_huge: c.per[1].rejected_victim,
        cache_bytes_small: c.per[0].bytes,
        cache_bytes_huge: c.per[1].bytes,
        cache_borrowed_small: c.per[0].borrowed_bytes,
        cache_borrowed_huge: c.per[1].borrowed_bytes,
        alloc_slots_small: small.1,
        alloc_slots_huge: huge.1,
        alloc_free_small: small.0,
        alloc_free_huge: huge.0,
        ..metrics::Sample::default()
    };
    // Groups in flight, per core and disjoint, so the sum over rows is the node's total.
    let (replaying, shedding) = d.heal.outstanding();
    s.heal_replaying = replaying;
    s.heal_shedding = shedding;
    // Pressure is not summable: each core votes, so the series counts cores in that state.
    match a.pressure() {
        Pressure::Low => s.alloc_pressure_low = 1,
        Pressure::Critical => s.alloc_pressure_critical = 1,
        Pressure::Normal => {}
    }
    if core == 0 {
        let cfg = d.config();
        s.alloc_quarantined = a.quarantined as u64;
        // A reload can add an extent the store has no slots for; growing needs a restart,
        // so report the shortfall instead of a silent ENOSPC when the free lists run down.
        s.alloc_unbacked = layout::shortfall(&a.geometry(), cfg);
        // Tail bytes and the part nobody holds, both node-wide. A large gap means cache
        // space that `policy.cache_index_bytes` will not pay to index.
        let (tail, unused) = d.cache.tail_bytes();
        s.cache_tail_bytes = tail;
        s.cache_unused_bytes = unused;
        // Device-wide rather than per core, so only one worker reports it.
        s.store_throttle_us = a.store_waited_us();
        s.config_generation = cfg.generation;
        s.config_rejected = config::rejected();
        // Epochs are per universe; a scalar gauge can only carry the largest.
        s.topology_epoch = cfg.universes().iter().map(|u| u.epoch).max().unwrap_or(0) as u64;
        s.node_id = cfg.node.id as u64;
        s.workers = a.cores() as u64;
        s.universes = cfg.universes().len() as u64;
        s.devices = cfg.devices().len() as u64;
        s.extents = cfg.extent_count() as u64;
        s.peers = cfg.peer_count() as u64;
    }
    metrics::publish(core, &s);
    // Per extent, outside `Sample` because rows exist only for extents the config names.
    // Every named extent gets a row even when this core holds nothing: the control plane
    // gates an epoch advance on a zero, and a missing series is not a zero.
    let cfg = d.config();
    let census = a.census();
    let rows: Vec<(u32, u32, u64, u64)> = cfg
        .extents()
        .map(|(u, e)| match census.binary_search_by_key(&e.id, |c| c.0) {
            Ok(i) => (u.id, e.id, census[i].1, census[i].2),
            Err(_) => (u.id, e.id, 0, 0),
        })
        .collect();
    metrics::publish_extents(core, &rows);
}

/// The consumer path: every mutation is a guarded accept, every read a quorum read.
/// Consensus owns the guard, the ballot and the fan-out, leaving address arithmetic.
///
/// A device is a concatenation of whole extents, so its block number becomes an address
/// here. Nothing else knows the mount order, which lets two hosts mount differently.
/// Extents carry their own page size, so one request may cross pages of both classes; it
/// is cut on the pages it covers and each piece is one operation. The whole cut is checked
/// before any of it runs, so a request that is not serviceable everywhere changes nothing.
async fn serve(d: &Dataplane, req: Request) -> Result<(), Status> {
    let a = d.alloc();
    let px = d.paxos;
    let cfg = d.config();
    let dev = cfg.device(req.dev as u32).ok_or(Status::Unmapped)?;
    let mut check = Segments::new(dev, &req);
    while let Some(s) = check.next() {
        s?.serviceable(req.op)?;
    }

    // At the low watermark we slow completions instead of failing. The tag stays
    // outstanding, so blk-mq's queue depth bounds what the kernel hands us next.
    if req.op == Op::Write && a.pressure() == Pressure::Low {
        sleep(Duration::from_micros(200)).await;
    }

    // Sequential: pieces of one request share nothing, but a 4 MiB run of 4 KiB pages is
    // 1024 rounds, and letting them all go at once would deepen every queue below.
    let mut segs = Segments::new(dev, &req);
    while let Some(s) = segs.next() {
        let s = s?;
        let addr = GlobalAddr(s.addr);
        if s.huge {
            match req.op {
                Op::Read => huge_read(d, addr, s.off, req.buf.slice(s.at, s.len)).await?,
                // A partial write would need a read-modify-write, which this class does
                // not do, and `serviceable` already refused one.
                Op::Write => px
                    .write(addr, Page::Huge(req.buf.slice(s.at, s.len)))
                    .await
                    .map(drop)?,
                Op::Discard => px.trim(addr).await?,
            }
        } else {
            match req.op {
                // Staged through registered memory so the checksum covers stable bytes.
                Op::Read => {
                    let mut page = PoolBuf::alloc(4096).await;
                    match px.read(addr, Sink::Small(&mut page)).await {
                        Ok(_) => {}
                        Err(Status::Hole) => page.fill(0),
                        Err(e) => return Err(e),
                    }
                    req.store(s.at, &page).map_err(|_| Status::Io)?;
                }
                Op::Write => {
                    let mut page = PoolBuf::alloc(4096).await;
                    req.load(s.at, &mut page).map_err(|_| Status::Io)?;
                    px.write(addr, Page::Small(&page)).await.map(drop)?;
                }
                Op::Discard => px.trim(addr).await?,
            }
        }
    }
    Ok(())
}

/// One piece of a request: all of it that falls in a single page.
struct Seg {
    /// The page's global address.
    addr: u64,
    /// A 4 MiB page.
    huge: bool,
    /// Byte offset of this piece within its page.
    off: usize,
    /// Byte offset of this piece within the request buffer.
    at: usize,
    len: usize,
}

impl Seg {
    /// Whether this piece is something the page under it can be asked to do. A 4 MiB page
    /// is written and trimmed whole or not at all: there is no read-modify-write in that
    /// class, and a trim frees the whole allocator page whatever range was handed to it.
    /// Reads are free to take a piece, since a `GET` serves one locally.
    fn serviceable(&self, op: Op) -> Result<(), Status> {
        let whole = self.off == 0 && self.len as u64 == layout::HUGE_PAGE;
        match op {
            Op::Read => Ok(()),
            _ if !self.huge || whole => Ok(()),
            _ => Err(Status::Unmapped),
        }
    }
}

/// Cuts a request into [`Seg`]s. The block layer is told a chunk size only where every
/// page of the device is the same, so on a mixed device this is the only thing that knows
/// where the boundaries are.
struct Segments<'a> {
    dev: &'a Device,
    lba: u64,
    at: usize,
    len: usize,
}

impl<'a> Segments<'a> {
    fn new(dev: &'a Device, req: &Request) -> Segments<'a> {
        Segments {
            dev,
            lba: req.lba,
            at: 0,
            len: req.buf.len(),
        }
    }

    // Not `Iterator`: it is consumed inside an `async` block, where a `for` over a
    // fallible iterator would have to collect first.
    #[allow(clippy::should_implement_trait)]
    fn next(&mut self) -> Option<Result<Seg, Status>> {
        if self.at >= self.len {
            return None;
        }
        // A device is addressed in 4 KiB blocks, so anything else is a request the kernel
        // should never have built out of the geometry we published.
        if !(self.len - self.at).is_multiple_of(4096) {
            return Some(Err(Status::Unmapped));
        }
        let Some(p) = self.dev.map(self.lba) else {
            return Some(Err(Status::Unmapped));
        };
        // A discard carries no payload to slice: it is a byte range like any other, but
        // the pages it names are freed whole.
        let run = (p.run() * 4096) as usize;
        let len = run.min(self.len - self.at);
        let seg = Seg {
            addr: p.addr,
            huge: p.huge,
            off: p.byte_off(),
            at: self.at,
            len,
        };
        self.lba += (len / 4096) as u64;
        self.at += len;
        Some(Ok(seg))
    }
}

/// The 4 MiB read path.
///
/// Immutable-only (enforced by config validation) and a Live immutable page is terminal
/// within its epoch, so it needs no round. A partial read is served locally only: a `GET`
/// names a page, not a byte range. A cache hit takes a confirming round, run beside the
/// cached read, so a hit costs one round trip and no 4 MiB page crosses the wire.
async fn huge_read(d: &Dataplane, addr: GlobalAddr, off: usize, buf: Buf) -> Result<(), Status> {
    let px = d.paxos;
    // The cache can only move whole pages: a `GET` has no trailer to carry an offset.
    let whole = off == 0 && buf.len() as u64 == layout::HUGE_PAGE;
    let w = if whole { px.cache_width(addr).await } else { 0 };
    if px.cached_huge(addr, off, w, buf).await {
        return Ok(());
    }
    // A hole served as zeroes has no version to offer, and nothing to gain from the cache
    // either: the zero region is one device read away on every node.
    if let Some(r) = huge_group(d, addr, off, buf).await?
        && whole
    {
        px.offer_huge(addr, w, buf, r.version).await;
    }
    Ok(())
}

/// The group's answer for a 4 MiB page, once the cache has had its say.
///
/// `None` where the group's answer is that nobody wrote the page: the bytes are zeroes and
/// there is no register to speak of. Shared with the cross-zone `GET` (below), which owes
/// its reader exactly what a reader in this zone would have seen.
async fn huge_group(
    d: &Dataplane,
    addr: GlobalAddr,
    off: usize,
    buf: Buf,
) -> Result<Option<Register>, Status> {
    let a = d.alloc();
    let px = d.paxos;
    let whole = off == 0 && buf.len() as u64 == layout::HUGE_PAGE;
    match a.read_huge(addr, off, buf).await {
        // Not an acceptor, so a local miss says nothing about existence: the bytes live
        // in the group and repair would only heal the members. Whole pages only.
        Err(Status::Hole | Status::Missing) if whole && !px.member_of(addr) => {
            match px.pull_huge(addr, buf).await {
                Err(Status::Hole) => a.read_zeroes(buf).await.map(|()| None),
                r => r.map(Some),
            }
        }
        Err(Status::Hole | Status::Missing) if px.healable(addr) => {
            px.repair(addr).await?;
            match a.read_huge(addr, off, buf).await {
                Err(Status::Hole) => a.read_zeroes(buf).await.map(|()| None),
                r => r.map(Some),
            }
        }
        // A hole reads as zeroes, and we may not touch the guest's pages, so they come
        // from the device's format-time zero region.
        Err(Status::Hole) => a.read_zeroes(buf).await.map(|()| None),
        r => r.map(Some),
    }
}

// --- fabric target ---

/// Serve one fabric command.
///
/// The LBA is the request, so decoding it is all of parsing and happens before anything
/// touches allocator state. [`Cmd::decode`] is total: an unknown or malformed frame is a
/// status, not a fault, and what comes back out is typed, so no handler below reads a
/// field the opcode does not define. A command need not land on the core owning its
/// consensus group (queue-to-core mapping is nvmet's business); the allocator's own hop
/// shards by group hash, so no hop is added here.
async fn dispatch(d: &Dataplane, universe: u32, req: Request) -> Result<(), Errno> {
    // The block layer never inverts direction, so the direction is part of the request:
    // a command that disagrees with its opcode was built for another address than the
    // peer meant.
    let cmd = Cmd::decode(req.lba, req.buf.len(), req.op == Op::Read)?;

    // Routing is decided before the opcode. A routable command names its addressee;
    // everything else is ours by construction, addressed by the sender's choice of link.
    if let Some((page, via)) = cmd.routing() {
        let addr = addr_of(d, universe, page)?;
        if !d.paxos.serves(cmd.op(), addr, via.to) {
            return relay(d, cmd, addr, req).await;
        }
    }

    match cmd {
        Cmd::Get { page, from, want } => get(d, universe, page, from, want, req).await,
        Cmd::GetMeta { page, from } => get_meta(d, universe, page, from, req).await,
        Cmd::Prepare { page, .. } => prepare(d, universe, page, req).await,
        Cmd::Accept { page, via, put } => accept(d, universe, page, via.to, put, req).await,
        Cmd::Trim { page, via } => trim(d, universe, page, via.to, req).await,
        Cmd::Learn { page, .. } => learn(d, universe, page, req).await,
        Cmd::Seal { .. } => seal(d, universe, req).await,
        Cmd::Warm { page } => warm(d, universe, page, req).await,
        Cmd::Merkle { group, class } => merkle(d, universe, group, class, req).await,
        Cmd::SnapOpen {
            group,
            class,
            bucket,
        } => snap_open(d, universe, group, class, bucket, req).await,
        Cmd::SnapNext { cursor, seq } => snap_next(d, universe, cursor, seq, req).await,
        Cmd::Term { group } => term(d, universe, group, req).await,
        Cmd::Ping => ping(d, universe, req).await,
    }
}

/// Page reference to allocator address.
///
/// The universe is the namespace the command arrived on, never anything the sender wrote,
/// so a peer given one universe's namespace cannot name a page in another. The extent
/// lookup is the bounds check: an address no extent covers is a bad frame, not a misrouted
/// one (routing happens above), and so is one whose page class disagrees with the
/// extent's, which would otherwise read a 4 MiB page as a 4 KiB one.
fn addr_of(d: &Dataplane, universe: u32, page: PageRef) -> Result<GlobalAddr, Errno> {
    let addr = config::addr_of(universe, page.lba());
    let e = d.config().extent_at(addr).ok_or(status::BAD)?;
    if Class::of(e.huge) != page.class() {
        return Err(status::BAD);
    }
    Ok(GlobalAddr(addr))
}

/// The consensus group a group-addressed command names, checked against its universe.
///
/// The wire carries only an index into the universe's catalog and the universe half comes
/// from the namespace, so a group id cannot be forged across the boundary. An index past
/// the catalog is a bad frame.
fn group_of(d: &Dataplane, universe: u32, group: GroupIx) -> Result<config::GroupId, Errno> {
    let u = d.config().universe(universe).ok_or(status::BAD)?;
    if group.get() as usize >= u.catalog.len() {
        return Err(status::BAD);
    }
    Ok(config::GroupId::new(universe, group.get()))
}

/// Allocator outcome to fabric status.
///
/// `Conflict` covers a CAS conflict and an already-written Immutable page alike; only four
/// statuses survive nvmet and the caller lost a race either way.
fn wire(s: Status) -> Errno {
    match s {
        Status::Hole | Status::Missing => status::MISSING,
        Status::Conflict { .. } => status::STALE,
        Status::Unmapped => status::BAD,
        Status::NoSpace => status::NOSPC,
        Status::Io => Errno::EIO,
    }
}

/// Store a reply trailer at `off` in the request buffer. The only way a footer reaches
/// the wire: it is encoded into a zeroed block, so no slot the record does not define
/// carries anything.
async fn reply(req: &Request, off: usize, f: &impl Footer) -> Result<(), Errno> {
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    f.encode(&mut t)?;
    req.store(off, &t)
}

/// Load and decode a request trailer. A malformed record is [`status::BAD`], and nothing
/// downstream ever sees the bytes.
async fn footer<F: Footer>(req: &Request) -> Result<F, Errno> {
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    req.load(0, &mut t)?;
    F::decode(&t)
}

/// `GET`: the reply *is* the payload.
async fn get(
    d: &Dataplane,
    universe: u32,
    page: PageRef,
    from: Source,
    want: Want,
    req: Request,
) -> Result<(), Errno> {
    let a = d.alloc();
    let addr = addr_of(d, universe, page)?;
    // A cached read never reaches the allocator; declining costs the reader one `GET`.
    let via = match from {
        Source::Cache => return cache_get(d, page.class(), want, addr, req).await,
        Source::Group(via) => via,
    };
    // `To::Owner` is a reader that resolved only our zone, so it wants the group's answer
    // rather than this node's. 4 KiB takes a round here; the 4 MiB class is immutable and
    // takes none, but a hole is still the group's to answer and not a miss to report.
    if via.to == To::Owner {
        if page.class() == Class::Small {
            return get_confirmed(d, want, addr, req).await;
        }
        if let Want::Piece { off } = want {
            return huge_group(d, addr, off, req.buf)
                .await
                .map(|_| ())
                .map_err(wire);
        }
    }
    if let Want::Piece { off } = want {
        // A hole here is not zeroes: answer that this member lacks the page, and let
        // consensus heal from another.
        return match a.read_huge(addr, off, req.buf).await {
            Err(Status::Hole) => Err(status::MISSING),
            r => r.map(|_| ()).map_err(wire),
        };
    }
    let mut p = PoolBuf::alloc(fabric::BLOCK).await;
    let reg = a.read_small(addr, &mut p).await.map_err(wire)?;
    req.store(0, &p)?;
    if want == Want::Gather {
        // The register rides along, so no separate `GETMETA`; the width hint costs a byte.
        let meta = d.paxos.gathered(addr, reg).await.map_err(wire)?;
        return reply(&req, fabric::BLOCK, &meta).await;
    }
    Ok(())
}

/// `GET` addressed to the owner: answer for the group rather than for ourselves.
///
/// A reader in another zone resolves only the zone and cannot fan out a hedged read, so
/// the round runs here: one round trip instead of three, metadata legs inside the zone.
async fn get_confirmed(
    d: &Dataplane,
    want: Want,
    addr: GlobalAddr,
    req: Request,
) -> Result<(), Errno> {
    let mut page = PoolBuf::alloc(fabric::BLOCK).await;
    // A hole is the group's answer here, not this node's miss: the round ran on this side
    // of the fabric, so the reader sees exactly what it would have seen had the page been
    // homed in its own zone. The wire cannot say "hole", and [`status::MISSING`] would
    // reach that reader's guest as `EIO` rather than as the zeroes a hole owes it.
    let r = match d.paxos.read_for(addr, Sink::Small(&mut page)).await {
        Ok(r) => r,
        Err(Status::Hole) => {
            page.fill(0);
            Register::default()
        }
        Err(e) => return Err(wire(e)),
    };
    req.store(0, &page)?;
    if want == Want::Gather {
        // The round confirmed this register. No width and no state: the hint belongs to
        // the sketch's owner and the reader has the value itself.
        let meta = fabric::MetaReply {
            reg: r.into(),
            ..Default::default()
        };
        return reply(&req, fabric::BLOCK, &meta).await;
    }
    Ok(())
}

/// `GET` from [`Source::Cache`].
///
/// Whatever this node holds as a cohort replica. There must be no fallback: a miss is
/// cheap and lands back at the reader, which then asks the group.
async fn cache_get(
    d: &Dataplane,
    class: Class,
    want: Want,
    addr: GlobalAddr,
    req: Request,
) -> Result<(), Errno> {
    // A shedding replica owes an `EAGAIN`, but only four errnos survive nvmet, so answer
    // `MISSING`; the reader's fallback is the same. The cache has no correctness role.
    if d.cache.shedding() {
        return Err(status::MISSING);
    }
    if let Want::Piece { off } = want {
        // The reader confirms this against the group. We owe a single entry, so the
        // version filter matches the paired `GETMETA`: no two different fills of a page.
        return match d.cache.load_immutable(addr, true, off, req.buf).await {
            Some(_) => Ok(()),
            None => Err(status::MISSING),
        };
    }
    // A 4 KiB cached read must gather: the register the copy claims has nowhere else to
    // ride, and the reader may not use the bytes without it.
    if want != Want::Gather || class != Class::Small {
        return Err(status::BAD);
    }
    let mut page = PoolBuf::alloc(2 * fabric::BLOCK).await;
    let buf = page.buf().slice(0, fabric::BLOCK);
    let r = d
        .cache
        .load(addr, false, 0, buf)
        .await
        .ok_or(status::MISSING)?;
    // The entry's claimed register travels with it; the reader confirms it against the
    // quorum and drops the entry on mismatch. No width: we are a replica, not the owner.
    let meta = fabric::MetaReply {
        reg: r.into(),
        ..Default::default()
    };
    meta.encode(&mut page[fabric::BLOCK..])?;
    req.store(0, &page)
}

/// `TRIM`: delete a page. A guarded accept with a tombstone value, so repeats are free.
async fn trim(
    d: &Dataplane,
    universe: u32,
    page: PageRef,
    to: To,
    req: Request,
) -> Result<(), Errno> {
    let addr = addr_of(d, universe, page)?;
    let t: fabric::TrimReq = footer(&req).await?;
    d.paxos.accept_trim(addr, to, t).await.map_err(wire)
}

/// `ACCEPT`: apply a page under a ballot. A 4 KiB command gathers guard and ballot into a
/// trailer beside the page; a 4 MiB one is all payload, so the acceptor derives both.
/// Legal only because 4 MiB pages are Immutable, whose guard comes from the config alone.
async fn accept(
    d: &Dataplane,
    universe: u32,
    page: PageRef,
    to: To,
    put: Put,
    req: Request,
) -> Result<(), Errno> {
    let addr = addr_of(d, universe, page)?;
    if let Put::Piece { off } = put {
        // A transport splits a 4 MiB command at MDTS, so the page can arrive in pieces.
        if off == 0 && req.buf.len() as u64 == layout::HUGE_PAGE {
            return d
                .paxos
                .accept(addr, to, None, Page::Huge(req.buf))
                .await
                .map_err(wire);
        }
        return d
            .paxos
            .accept_part(addr, to, off as u32, req.buf)
            .await
            .map_err(wire);
    }
    let mut both = PoolBuf::alloc(2 * fabric::BLOCK).await;
    req.load(0, &mut both)?;
    let (page, trailer) = both.split_at(fabric::BLOCK);
    let t = fabric::AcceptReq::decode(trailer)?;
    // Staged into our own memory so the checksum covers stable bytes, as `serve` does.
    let mut p = PoolBuf::alloc(fabric::BLOCK).await;
    p.copy_from_slice(page);
    d.paxos
        .accept(addr, to, Some(t), Page::Small(&p))
        .await
        .map_err(wire)
}

/// `GETMETA`: the metadata half of a hedged read: a register, and no page bytes.
///
/// From [`Source::Cache`] it answers for our cached copy instead: the 4 MiB class has no
/// trailer to gather a register into, so it rides a second command beside the page.
async fn get_meta(
    d: &Dataplane,
    universe: u32,
    page: PageRef,
    from: Source,
    req: Request,
) -> Result<(), Errno> {
    let addr = addr_of(d, universe, page)?;
    if from == Source::Cache {
        if d.cache.shedding() {
            return Err(status::MISSING);
        }
        let r = d
            .cache
            .peek_immutable(addr, page.class() == Class::Huge)
            .await
            .ok_or(status::MISSING)?;
        // No width: we are a replica here, not the owner.
        let meta = fabric::MetaReply {
            reg: r.into(),
            ..Default::default()
        };
        return reply(&req, 0, &meta).await;
    }
    let meta = d.paxos.get_meta(addr).await.map_err(wire)?;
    reply(&req, 0, &meta).await
}

/// `PREPARE`: raise this group's promise and report what we hold. A read carries no body,
/// so the term is not on the wire: raise ours by one, the preparer takes the maximum.
async fn prepare(d: &Dataplane, universe: u32, page: PageRef, req: Request) -> Result<(), Errno> {
    let addr = addr_of(d, universe, page)?;
    let p = d.paxos.prepare(addr).await.map_err(wire)?;
    reply(&req, 0, &p).await
}

/// `LEARN`: a value we may be behind on, and the member holding it. Apply-if-newer, so
/// a repeat is free and the repair and migration streams commute.
async fn learn(d: &Dataplane, universe: u32, page: PageRef, req: Request) -> Result<(), Errno> {
    let addr = addr_of(d, universe, page)?;
    let l: fabric::LearnReq = footer(&req).await?;
    d.paxos
        .learn(addr, l.reg.into(), l.from.index(), l.repair)
        .await
        .map_err(wire)
}

/// `WARM`: another zone wrote a page this zone asked to keep warm.
///
/// Advisory in both directions, so it always succeeds; the sender has already moved on.
async fn warm(d: &Dataplane, universe: u32, page: PageRef, req: Request) -> Result<(), Errno> {
    let addr = addr_of(d, universe, page)?;
    let w: fabric::WarmReq = footer(&req).await?;
    d.paxos.warm(addr, w.version, w.stage).await;
    Ok(())
}

/// `SEAL`: freeze a shard at its source group. An ordinary accept over a shard.
///
/// The command's page is a routing anchor only; the shard it names is in the footer.
async fn seal(d: &Dataplane, universe: u32, req: Request) -> Result<(), Errno> {
    let s: fabric::SealReq = footer(&req).await?;
    // An extent id is unique across universes, so it names the shard alone; this check
    // stops a peer sealing a shard outside its own partition.
    let ours = d
        .config()
        .extent_by_id(s.extent)
        .is_some_and(|(u, _)| u.id == universe);
    if !ours {
        return Err(status::BAD);
    }
    d.paxos.seal(s.extent, s.term).await.map_err(wire)
}

/// `MERKLE`: our digest vector for one group and class.
///
/// The three anti-entropy ops name a consensus group, not a page, so `addr_of` is off
/// these paths. A group we hold nothing for answers zeroes, the digest of an empty set,
/// not an error.
async fn merkle(
    d: &Dataplane,
    universe: u32,
    group: GroupIx,
    class: Class,
    req: Request,
) -> Result<(), Errno> {
    let group = group_of(d, universe, group)?;
    let v = d.alloc().digests(group, class == Class::Huge).await;
    reply(&req, 0, &fabric::MerkleReply(v)).await
}

/// `SNAPOPEN`: begin enumerating a group's registers, optionally filtered to one digest
/// bucket. The reply is the cursor id, which is self-describing so `SNAPNEXT` needs
/// nothing else. `NOSPC` means this slab already holds as many cursors as it will.
async fn snap_open(
    d: &Dataplane,
    universe: u32,
    group: GroupIx,
    class: Class,
    bucket: Option<fabric::Bucket>,
    req: Request,
) -> Result<(), Errno> {
    let group = group_of(d, universe, group)?;
    let filter = bucket.map_or(heal::Filter::All, |b| heal::Filter::Bucket(b.get()));
    let id = d
        .alloc()
        .snap_open(group, class == Class::Huge, filter)
        .await
        .map_err(wire)?;
    let r = fabric::SnapOpenReply {
        cursor: fabric::Cursor::new(id),
    };
    reply(&req, 0, &r).await
}

/// `SNAPNEXT`: the next chunk of `(address, version, ballot)`, and no page bytes.
///
/// The sequence number makes a retry idempotent; a cursor that skipped a chunk would
/// under-report a difference. The last chunk closes the cursor (no close on the wire), so
/// a lost final reply is not retryable: the reader repeats the bucket next pass.
async fn snap_next(
    d: &Dataplane,
    universe: u32,
    cursor: fabric::Cursor,
    seq: fabric::Seq,
    req: Request,
) -> Result<(), Errno> {
    let id = cursor.get();
    // Cursor ids are small integers, so check the opening universe against the caller's.
    let (tuples, done) = d
        .alloc()
        .snap_next(id, Some(seq.get()), Some(universe))
        .await
        .map_err(wire)?;
    let tuples = tuples
        .iter()
        .map(|&(addr, r)| fabric::SnapTuple {
            addr,
            reg: r.into(),
        })
        .collect();
    // The allocator is asked for at most a chunk's worth, so a longer run is a bug here
    // rather than something to truncate onto the wire.
    let r = fabric::SnapNextReply::new(done, tuples).ok_or(status::BAD)?;
    if done {
        d.alloc().snap_release(id).await;
    }
    reply(&req, 0, &r).await
}

/// `TERM`: the promise we hold for one group, for a member rebuilding its own. A group we
/// hold nothing for answers zero, which cannot lower the max taken.
async fn term(d: &Dataplane, universe: u32, group: GroupIx, req: Request) -> Result<(), Errno> {
    let group = group_of(d, universe, group)?;
    let t = d.paxos.term(group).await.map_err(wire)?;
    reply(&req, 0, &t).await
}

/// `PING`: liveness plus the geometry that dates an answer. The epoch is the arriving
/// universe's; a caller has no business learning another's.
async fn ping(d: &Dataplane, universe: u32, req: Request) -> Result<(), Errno> {
    let c = d.config();
    let p = fabric::PingReply {
        node: c.node.id,
        generation: c.generation,
        epoch: c.universe(universe).map_or(0, |u| u.epoch),
    };
    reply(&req, 0, &p).await
}

/// A command that is not ours: reissue it on our own link and complete when it does.
///
/// We hold only the in-flight future: no session, no buffering, no copy. The command
/// keeps its shape, so one registered buffer serves both hops; if we die the outer command
/// fails and the originator reads a timeout. The budget terminates the recursion: each hop
/// spends one and a foreign command with none left is refused, which also bounds a
/// migration forward chain. Only an already-decoded command may be relayed, so a
/// forwarder passes on nothing it could not itself have parsed. The link picked is one of
/// the arriving universe's, so a relay stays in that universe.
async fn relay(d: &Dataplane, cmd: Cmd, addr: GlobalAddr, req: Request) -> Result<(), Errno> {
    // No link to the member: the originator has our placement wrong, so send it back to
    // its config rather than report the page missing. `Link::relay` does the same for an
    // exhausted budget.
    let (_, via) = cmd.routing().ok_or(status::BAD)?;
    let link = d
        .paxos
        .forward_link(cmd.op(), addr, via.to)
        .ok_or(status::STALE)?;
    link.relay(cmd, req.buf).await
}

// --- Tests ---

/// The whole stack through a real ublk block device: format, serve, write, read, discard,
/// most fabric frames (`TERM` is not exercised), and the corruption asymmetry as seen by
/// the block layer.
///
/// In the crate rather than `tests/` so the allocator can stay private. Needs root and a
/// working ublk subsystem: it creates and destroys devices. One dataplane, one process,
/// one boot; `tests/cluster.rs` restarts a real process.
#[cfg(test)]
mod tests {
    use std::io::{Read, Seek, SeekFrom, Write};
    use std::os::fd::AsRawFd;
    use std::path::{Path, PathBuf};
    use std::sync::{Arc, Mutex};

    use super::{self as server, SERVER};
    use crate::alloc::GlobalAddr;
    use crate::config::Config;
    use crate::fabric::{self, Footer};
    use crate::heal;
    use crate::layout::{self, Class, State};
    use crate::runtime;

    const IMG: &str = "racer-e2e-alloc.img";
    const DEV_BYTES: u64 = 1 << 30;

    /// One universe, laid out by hand: extent 1 is LWW, extent 2 is OCC, extent 3 is
    /// immutable 4 MiB. Eight groups so addresses spread over workers and the cross-core
    /// paths are used. Extent 4 is homed in zone 2, which this node is not in and holds
    /// no link to, so a foreign address is routed rather than resolved. Extent 5 is homed
    /// here but on its way to zone 2, so it serves locally until sealed and forwards
    /// afterwards. The gap between extents 2 and 3 is deliberate: an LBA in it belongs to
    /// no extent, which is what a frame addressing past the end of anything has to hit.
    /// Extent 3 starts 1024-aligned because a 4 MiB page covers 1024 blocks.
    ///
    /// Devices 1 to 5 each export one extent. Device 6 exports extents 2 and 1 in that
    /// order: a device is an arbitrary ordered set of extents, so the same extent may be
    /// mounted twice at a different offset, and the address resolves to the extent's, not
    /// the device's. Device 7 puts a 4 KiB extent and a 4 MiB one behind one export, so
    /// the page size of a request is decided by where in the device it lands. Extent 6 is
    /// its own so the whole-page fills below do not collide with extent 3's.
    ///
    /// This node is a member of every group; with no peers declared, a group we are not
    /// in has no reachable member. A single node is member index 0 everywhere, so its
    /// quorum is one and a local accept is a decision. Being in every group also pins the
    /// zone to three nodes, since an equal share of eight groups over anything wider
    /// would leave us holding more of the zone than the rest.
    fn config_text(dev: &Path) -> String {
        format!(
            "
            generation 1
            node id=1 zone=1 store={} size={DEV_BYTES}
            universe 1 fabric_device_id=8
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            zone id=2 gateways=2,3,4
            extent id=1 base=0 pages=4096 kind=lww zone=1
            extent id=2 base=4096 pages=256 kind=occ zone=1
            extent id=3 base=5120 pages=2 kind=immutable_4m zone=1
            extent id=4 base=7168 pages=64 kind=lww zone=2
            extent id=5 base=7232 pages=64 kind=lww zone=1 next_zone=2
            extent id=6 base=8192 pages=2 kind=immutable_4m zone=1
            device 1 extents=1
            device 2 extents=2
            device 3 extents=3
            device 4 extents=4
            device 5 extents=5
            device 6 extents=2,1
            device 7 extents=1,6
            ",
            dev.display()
        )
    }

    /// Probe the control node: a kernel built without `ublk_drv` refuses these tests
    /// however privileged we are, so "am I root" is not the precondition.
    fn privileged() -> bool {
        std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open("/dev/ublk-control")
            .is_ok()
    }

    fn img() -> PathBuf {
        std::env::temp_dir().join(IMG)
    }

    /// Where the allocator put a page, read straight out of the metadata region, so a
    /// page's bytes can be corrupted behind the allocator's back.
    fn slot_of(dev: &Path, class: Class, addr: GlobalAddr) -> Option<u32> {
        let geo = layout::read_geometry(dev).unwrap();
        let f = layout::open_direct(dev, false).unwrap();
        let mut buf = layout::Aligned::new(layout::MBLOCK);
        for id in 0..geo.mblocks(class) as u32 {
            // Whichever copy is current, the same way startup does.
            let mut best: Option<(u64, [u8; layout::MBLOCK])> = None;
            for copy in 0..2u8 {
                layout::read_at(&f, buf.as_mut(), geo.mblock_off(class, id, copy)).unwrap();
                if let Some(h) = layout::get_header(buf.as_ref())
                    && best.as_ref().is_none_or(|(g, _)| h.generation > *g)
                {
                    let mut raw = [0u8; layout::MBLOCK];
                    raw.copy_from_slice(buf.as_ref());
                    best = Some((h.generation, raw));
                }
            }
            let Some((_, raw)) = best else { continue };
            for i in 0..class.k() {
                let e = layout::get_entry(&raw, class, i);
                if e.addr == addr.0 && e.state == State::Live {
                    return Some(id * class.k() + i);
                }
            }
        }
        None
    }

    fn flip_byte(dev: &Path, off: u64) {
        let f = layout::open_direct(dev, true).unwrap();
        let mut buf = layout::Aligned::new(4096);
        let page = off / 4096 * 4096;
        layout::read_at(&f, buf.as_mut(), page).unwrap();
        buf.as_mut()[(off % 4096) as usize] ^= 0xff;
        layout::write_at(&f, buf.as_ref(), page).unwrap();
    }

    /// Bring the dataplane up and hand back the ublk path of each declared device,
    /// ordered by device id, plus the path of universe 1's fabric device.
    fn up(cfg: Config) -> (runtime::Runtime<server::Dataplane>, Vec<PathBuf>, PathBuf) {
        let rt = runtime::start(&SERVER).expect("start");
        let found = Arc::new(Mutex::new((Vec::new(), PathBuf::new())));
        let out = found.clone();
        let node = server::Node::new();
        rt.reload(move |c| {
            let d = node.attach(c, cfg)?;
            let mut devs = d.devices();
            devs.sort_by_key(|(id, _)| *id);
            let fabs = d.fabrics();
            assert_eq!(fabs.len(), 1, "one namespace per universe");
            *out.lock().unwrap() = (
                devs.into_iter().map(|(_, p)| p).collect(),
                fabs[0].1.clone(),
            );
            Ok(d)
        })
        .expect("reload");
        let (paths, fab) = found.lock().unwrap().clone();
        for p in paths.iter().chain(std::iter::once(&fab)) {
            for _ in 0..200 {
                if p.exists() {
                    break;
                }
                std::thread::sleep(std::time::Duration::from_millis(10));
            }
        }
        (rt, paths, fab)
    }

    fn open_dev(p: &Path) -> std::fs::File {
        std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(p)
            .unwrap()
    }

    fn write_at(f: &mut std::fs::File, off: u64, data: &[u8]) {
        f.seek(SeekFrom::Start(off)).unwrap();
        f.write_all(data).unwrap();
        f.sync_data().unwrap();
    }

    fn read_at(f: &mut std::fs::File, off: u64, len: usize) -> std::io::Result<Vec<u8>> {
        let mut v = vec![0u8; len];
        f.seek(SeekFrom::Start(off))?;
        f.read_exact(&mut v)?;
        Ok(v)
    }

    fn pattern(seed: u8, len: usize) -> Vec<u8> {
        (0..len)
            .map(|i| seed ^ (i as u8).wrapping_mul(31))
            .collect()
    }

    /// A command issued against our own fabric device, as a peer's link would: the peer's
    /// namespace is this block device, so this exercises all but the transport itself.
    /// The command and the length together name the LBA, exactly as [`fabric::Link`]
    /// does, so a test cannot phrase a shape the wire has no room for.
    fn frame_read(f: &layout::Dev, cmd: fabric::Cmd, len: usize) -> std::io::Result<Vec<u8>> {
        let mut buf = layout::Aligned::new(len);
        let lba = cmd.encode(len).expect("a shape this op allows");
        layout::read_at(f, buf.as_mut(), lba * 4096)?;
        Ok(buf.as_ref().to_vec())
    }

    fn frame_write(f: &layout::Dev, cmd: fabric::Cmd, data: &[u8]) -> std::io::Result<()> {
        let mut buf = layout::Aligned::new(data.len());
        buf.as_mut().copy_from_slice(data);
        let lba = cmd.encode(data.len()).expect("a shape this op allows");
        layout::write_at(f, buf.as_ref(), lba * 4096)
    }

    /// The page a small or huge command names. Panics on an address the fabric cannot
    /// phrase, which in a test is a bug in the test.
    fn page_of(class: Class, lba: u64) -> fabric::PageRef {
        fabric::PageRef::new(class, lba).expect("an addressable page")
    }

    /// The ordinary "you own this" route: no addressee and no forwarding budget.
    fn owner() -> fabric::Via {
        fabric::Via::direct(fabric::To::Owner)
    }

    /// A group index the catalog may or may not contain; bounds are the target's business.
    fn group_ix(i: u32) -> fabric::GroupIx {
        fabric::GroupIx::new(i).expect("a representable group")
    }

    /// Fold one `SNAPNEXT` reply into `out`, answering whether the cursor is finished.
    fn tuples(t: &[u8], out: &mut std::collections::BTreeMap<u64, fabric::Reg>) -> bool {
        let chunk = fabric::SnapNextReply::decode(t).expect("a well-formed chunk");
        out.extend(chunk.tuples().iter().map(|t| (t.addr, t.reg)));
        chunk.done
    }

    fn errno(e: std::io::Error) -> i32 {
        e.raw_os_error().unwrap_or(0)
    }

    /// A read that has to reach the device; a buffered read would answer from bytes
    /// fetched before the disk underneath was corrupted.
    fn direct_read(p: &Path, off: u64, len: usize) -> std::io::Result<Vec<u8>> {
        let f = layout::open_direct(p, false)?;
        let mut b = layout::Aligned::new(len);
        layout::read_at(&f, b.as_mut(), off)?;
        Ok(b.as_ref().to_vec())
    }

    /// A write of exactly this shape: buffered writeback is free to cut a large write into
    /// whatever requests it likes, and where a 4 MiB page begins and ends is the point.
    fn direct_write(p: &Path, off: u64, data: &[u8]) -> std::io::Result<()> {
        let f = layout::open_direct(p, true)?;
        let mut b = layout::Aligned::new(data.len());
        b.as_mut().copy_from_slice(data);
        layout::write_at(&f, b.as_ref(), off)
    }

    /// One boot, three layers: the block client's view, a peer's view over the fabric, and
    /// a disk gone wrong from both. Needs the real kernel seams, which `sim` replaces.
    #[cfg(not(feature = "sim"))]
    #[test]
    fn dataplane_end_to_end() {
        let _only = runtime::exclusive();
        if !privileged() {
            eprintln!("skipping: ublk device creation needs /dev/ublk-control");
            return;
        }
        let dev = img();
        let _ = std::fs::remove_file(&dev);
        let cfg = Config::parse(&config_text(&dev)).unwrap();
        cfg.validate().unwrap();
        layout::size_if_needed(&dev, &cfg).unwrap();
        layout::format(&dev, &cfg).unwrap();

        let small = pattern(0xa5, 4096);
        let huge = pattern(0x5a, 4 << 20);

        let (rt, paths, fab_path) = up(Config::parse(&config_text(&dev)).unwrap());
        let mut lww = open_dev(&paths[0]);
        let mut imm = open_dev(&paths[2]);

        // ---- the block client: write, read back, discard, and the type semantics -------

        // Spread across enough pages that several groups, and so several cores, are hit.
        for p in 0..64u64 {
            write_at(&mut lww, p * 4096, &pattern(p as u8, 4096));
        }
        for p in 0..64u64 {
            assert_eq!(
                read_at(&mut lww, p * 4096, 4096).unwrap(),
                pattern(p as u8, 4096)
            );
        }

        // A never-written page is a hole and reads as zeroes.
        assert_eq!(
            read_at(&mut lww, 1000 * 4096, 4096).unwrap(),
            vec![0u8; 4096]
        );

        // Overwrite in place from the client's point of view; out of place underneath.
        write_at(&mut lww, 0, &small);
        assert_eq!(read_at(&mut lww, 0, 4096).unwrap(), small);

        // Discard is a trim; to a block client a trimmed page looks like one never written.
        let range = [63 * 4096u64, 4096u64];
        let rc = unsafe {
            libc::ioctl(
                lww.as_raw_fd(),
                0x1277, /* BLKDISCARD */
                range.as_ptr(),
            )
        };
        assert_eq!(rc, 0, "discard: {}", std::io::Error::last_os_error());
        assert_eq!(
            direct_read(&paths[0], 63 * 4096, 4096).unwrap(),
            vec![0u8; 4096]
        );

        // Immutable: the first whole-page fill lands, a second one is refused.
        write_at(&mut imm, 0, &huge);
        assert_eq!(read_at(&mut imm, 0, 4 << 20).unwrap(), huge);
        imm.seek(SeekFrom::Start(0)).unwrap();
        assert!(
            imm.write_all(&huge).and_then(|_| imm.sync_data()).is_err(),
            "CORFU fill must refuse a second write"
        );

        // ---- devices are compositions, not owners -------------------------------
        // Device 6 is extents 2 and 1 concatenated, so its page 256 is extent 1's page 0,
        // the same bytes device 1 shows at page 0. An address belongs to an extent; a
        // device is only an order of extents.
        let mut both_ext = open_dev(&paths[5]);
        assert_eq!(read_at(&mut both_ext, 0, 4096).unwrap(), vec![0u8; 4096]);
        assert_eq!(
            read_at(&mut both_ext, (256 + 2) * 4096, 4096).unwrap(),
            pattern(2, 4096)
        );
        // One page, not two copies: a write via the second mapping shows in the first.
        let shared = pattern(0xc3, 4096);
        write_at(&mut both_ext, (256 + 8) * 4096, &shared);
        assert_eq!(direct_read(&paths[0], 8 * 4096, 4096).unwrap(), shared);
        drop(both_ext);

        // ---- mixed page sizes: the class belongs to the request, not the device --
        // Device 7 is extent 1, of 4 KiB pages, followed by extent 6, of 4 MiB ones. Where
        // a request lands decides what it may be: anything 4 KiB aligned in the head, and
        // in the tail a read of any part of a page it has but a fill or a trim of the
        // whole page, since a 4 MiB page is allocated and freed as one.
        const SEAM: u64 = 4096 * 4096; // extent 1 is 4096 small pages
        const TAIL: u64 = SEAM + (4 << 20); // the second 4 MiB page of extent 6
        let mut mixed = open_dev(&paths[6]);
        assert_eq!(
            mixed.seek(SeekFrom::End(0)).unwrap(),
            SEAM + 2 * (4 << 20),
            "a device is as large as the extents behind it, whatever their pages"
        );

        // Nothing stops the block layer merging small pages, so a run of them arrives as
        // one request and becomes one operation each.
        let run = pattern(0x11, 16 * 4096);
        write_at(&mut mixed, 100 * 4096, &run);
        assert_eq!(direct_read(&paths[6], 100 * 4096, 16 * 4096).unwrap(), run);
        assert_eq!(
            direct_read(&paths[0], 107 * 4096, 4096).unwrap(),
            run[7 * 4096..8 * 4096].to_vec(),
            "the head is extent 1, whichever device reaches it"
        );

        // The tail is 4 MiB pages: a fill is the whole page or nothing, a read is free.
        write_at(&mut mixed, SEAM, &huge);
        assert_eq!(direct_read(&paths[6], SEAM, 4 << 20).unwrap(), huge);
        assert_eq!(
            direct_read(&paths[6], SEAM + 8 * 4096, 4096).unwrap(),
            huge[8 * 4096..9 * 4096].to_vec(),
            "a read may take any part of a page that is already there"
        );
        assert!(
            direct_write(&paths[6], TAIL, &small).is_err(),
            "4 KiB fills nothing of a 4 MiB page"
        );
        assert!(
            direct_write(&paths[6], TAIL, &huge[..2 << 20]).is_err(),
            "and neither does most of one"
        );

        // A request may cross the seam. Reads compose, because each piece is legal on its
        // own; a write does not, and is refused whole rather than half applied.
        let last = pattern(0x22, 4096);
        write_at(&mut mixed, SEAM - 4096, &last);
        let seam = direct_read(&paths[6], SEAM - 4096, 2 * 4096).unwrap();
        assert_eq!(seam[..4096].to_vec(), last);
        assert_eq!(seam[4096..].to_vec(), huge[..4096].to_vec());
        assert!(
            direct_write(&paths[6], SEAM - 4096, &pattern(0x33, 2 * 4096)).is_err(),
            "a write across the seam would fill a 4 MiB page with 4 KiB"
        );
        assert_eq!(
            direct_read(&paths[6], SEAM - 4096, 4096).unwrap(),
            last,
            "and the piece of it that was legal did not land either"
        );

        // Trim frees the page it names, so in the tail it too is a whole page or nothing.
        write_at(&mut mixed, TAIL, &huge);
        let part: [u64; 2] = [TAIL, 4096];
        assert_ne!(
            unsafe {
                libc::ioctl(
                    mixed.as_raw_fd(),
                    0x1277, /* BLKDISCARD */
                    part.as_ptr(),
                )
            },
            0,
            "a 4 KiB discard may not free a 4 MiB page"
        );
        assert_eq!(
            direct_read(&paths[6], TAIL, 4096).unwrap(),
            huge[..4096].to_vec()
        );
        let whole: [u64; 2] = [TAIL, 4 << 20];
        let rc = unsafe { libc::ioctl(mixed.as_raw_fd(), 0x1277, whole.as_ptr()) };
        assert_eq!(rc, 0, "discard: {}", std::io::Error::last_os_error());
        assert_eq!(
            direct_read(&paths[6], TAIL, 4096).unwrap(),
            vec![0u8; 4096],
            "a trimmed page reads as one never written"
        );
        drop(mixed);

        // ---- the fabric, against itself -----------------------------------------
        // The node's own fabric device, opened as if it were a peer's. Everything but the
        // two nvme hops is real: the same decode, dispatch and allocator. nvme-of is
        // transparent by construction, since the peer's namespace is this block device.
        //
        // Loopback cannot check the status alphabet and fails permissively, so the
        // assertions below are narrow: a loopback error goes `ublk -> BLK_STS_* -> errno`
        // and keeps roughly a dozen values, a real one goes on through
        // `nvmet -> NVMe -> initiator` and keeps four. Only the four in `fabric::status`
        // may be asserted on. A frame carries no extent and no device, only a block offset
        // in the universe's LBA space; which extent covers it is the control plane's.
        const LWW: u64 = 0; // extent 1
        const IMM: u64 = 5120; // extent 3, 1024-aligned because it is 4 MiB
        const AWAY: u64 = 7168; // extent 4, zone 2
        const GOING: u64 = 7232; // extent 5, zone 1 -> zone 2
        const GAP: u64 = 4500; // between extent 2 and extent 3: no extent at all

        // O_DIRECT so every frame is a real command, not a cached reply to another one.
        let fab = layout::open_direct(&fab_path, true).unwrap();
        write_at(&mut lww, 7 * 4096, &small);

        // ---- GET, bare: the reply is the payload -------------------------------
        let page7 = page_of(Class::Small, LWW + 7);
        let get7 = fabric::Cmd::Get {
            page: page7,
            from: fabric::Source::Group(owner()),
            want: fabric::Want::Page,
        };
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), small);

        // ---- GET, gather: page then trailer, one command ------------------------
        // The trailer is the register the page was chosen at: value and proof in one cmd.
        let gather7 = fabric::Cmd::Get {
            page: page7,
            from: fabric::Source::Group(owner()),
            want: fabric::Want::Gather,
        };
        let both = frame_read(&fab, gather7, 8192).unwrap();
        assert_eq!(&both[..4096], &small[..]);
        let meta = fabric::MetaReply::decode(&both[4096..]).expect("a well-formed register");
        let v7 = meta.reg.version;
        assert_ne!(v7, 0, "a page that exists was written at some version");
        assert_ne!(meta.reg.ballot, 0, "and at the ballot that chose it");

        // ---- A page nobody has: whose answer is it? -----------------------------
        // Asked of a named member, a hole is that member's miss, so consensus can heal it
        // from another copy rather than believe a page of zeroes.
        let gone = fabric::Cmd::Get {
            page: page_of(Class::Small, LWW + 999),
            from: fabric::Source::Group(fabric::Via::direct(fabric::To::Member(
                fabric::Member::new(0).unwrap(),
            ))),
            want: fabric::Want::Page,
        };
        let e = frame_read(&fab, gone, 4096).unwrap_err();
        assert_eq!(
            errno(e),
            libc::ENODATA,
            "a member that lacks the page says so, rather than inventing zeroes"
        );
        // Asked of the owner, the round ran on this side of the fabric and the answer is
        // the group's: a hole is zeroes, exactly as a reader in this zone would have seen.
        // Reporting the miss instead would reach that reader's guest as `EIO`.
        let gone = fabric::Cmd::Get {
            page: page_of(Class::Small, LWW + 999),
            from: fabric::Source::Group(owner()),
            want: fabric::Want::Page,
        };
        assert_eq!(
            frame_read(&fab, gone, 4096).unwrap(),
            vec![0u8; 4096],
            "a hole the group agrees on is the zeroes its reader is owed"
        );
        // Over the block device the same page still reads as zeroes, as a consumer needs.
        assert_eq!(
            read_at(&mut lww, 999 * 4096, 4096).unwrap(),
            vec![0u8; 4096]
        );

        // ---- Addressing: unmapped block, and the wrong page class ---------------
        // The address space is sparse: a gap between extents is as unmapped as an offset
        // past every extent, and neither is a hole.
        let gap = fabric::Cmd::Get {
            page: page_of(Class::Small, GAP),
            from: fabric::Source::Group(owner()),
            want: fabric::Want::Page,
        };
        assert_eq!(
            errno(frame_read(&fab, gap, 4096).unwrap_err()),
            libc::EOPNOTSUPP
        );
        let beyond = fabric::Cmd::Get {
            page: page_of(Class::Small, 1 << 30),
            from: fabric::Source::Group(owner()),
            want: fabric::Want::Page,
        };
        assert_eq!(
            errno(frame_read(&fab, beyond, 4096).unwrap_err()),
            libc::EOPNOTSUPP
        );
        let miscl = fabric::Cmd::Get {
            page: page_of(Class::Huge, LWW),
            from: fabric::Source::Group(owner()),
            want: fabric::Want::Piece { off: 0 },
        };
        assert_eq!(
            errno(frame_read(&fab, miscl, 4096).unwrap_err()),
            libc::EOPNOTSUPP
        );

        // ---- GET, 4 MiB: whole, and in the pieces an MDTS split would produce ----
        let pageh = page_of(Class::Huge, IMM);
        let piece = |off: usize| fabric::Cmd::Get {
            page: pageh,
            from: fabric::Source::Group(owner()),
            want: fabric::Want::Piece { off },
        };
        let geth = piece(0);
        assert_eq!(frame_read(&fab, geth, 4 << 20).unwrap(), huge);
        for (block, len) in [(0usize, 256 << 10), (64, 256 << 10), (1023, 4096)] {
            let mut b = layout::Aligned::new(len);
            let off = piece(block * 4096).encode(len).unwrap() * 4096;
            layout::read_at(&fab, b.as_mut(), off).unwrap();
            assert_eq!(
                b.as_ref(),
                &huge[block * 4096..block * 4096 + len],
                "block {block}"
            );
        }

        // ---- PING: liveness and the geometry that dates an answer ---------------
        let ping = fabric::Cmd::Ping;
        let t = frame_read(&fab, ping, 4096).unwrap();
        let pong = fabric::PingReply::decode(&t).expect("a well-formed ping reply");
        assert_eq!(pong.node, 1, "node id");
        assert_eq!(pong.generation, 1, "config generation");
        assert_eq!(pong.epoch, 0, "topology epoch");
        // Control ops are exactly one block, which the encoder will not even phrase.
        assert!(ping.encode(8192).is_err());

        // A small ACCEPT must gather page and trailer into one command; a page alone is bad.
        let acc7 = fabric::Cmd::Accept {
            page: page7,
            via: owner(),
            put: fabric::Put::Gather,
        };
        assert!(
            acc7.encode(4096).is_err(),
            "a bare page is not an accept the wire can carry"
        );

        // ---- GETMETA: the register, and not one byte of the page -----------------
        let meta7 = fabric::Cmd::GetMeta {
            page: page7,
            from: fabric::Source::Group(owner()),
        };
        let t = frame_read(&fab, meta7, 4096).unwrap();
        let m = fabric::MetaReply::decode(&t).expect("a well-formed register");
        assert_eq!(
            m.reg.version, v7,
            "the metadata read and the gathered trailer agree"
        );
        assert_ne!(
            m.reg.ballot, 0,
            "a chosen value carries the ballot that chose it"
        );

        // ---- ACCEPT: one round trip, and the guard is the collision detector ------
        // Page and trailer travel together, so a proposal is one command; `imm` zero says
        // the sender is not a member and we are the proposer.
        let next = pattern(0x11, 4096);
        let mut cmd = vec![0u8; 8192];
        cmd[..4096].copy_from_slice(&next);
        fabric::AcceptReq {
            guard: fabric::Guard::At(v7),
            ballot: 0,
            epoch: 0,
        }
        .encode(&mut cmd[4096..])
        .unwrap();
        frame_write(&fab, acc7, &cmd).expect("an accept at the version we hold");
        // Read back over the fabric: the block device fd is buffered.
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), next);

        assert_eq!(
            errno(frame_write(&fab, acc7, &cmd).unwrap_err()),
            libc::EREMOTEIO,
            "a guard that no longer matches is refused, not applied"
        );
        let t = frame_read(&fab, meta7, 4096).unwrap();
        assert_eq!(
            fabric::MetaReply::decode(&t).unwrap().reg.version,
            v7 + 1,
            "the loser left no trace"
        );

        // ---- PREPARE: raises this group's promise and reports the register -------
        let prep7 = fabric::Cmd::Prepare {
            page: page7,
            via: owner(),
        };
        let t = frame_read(&fab, prep7, 4096).unwrap();
        let p = fabric::PrepareReply::decode(&t).expect("a well-formed prepare reply");
        assert_eq!(p.reg.version, v7 + 1, "prepare reports, it does not write");
        let term1 = p.term;
        assert!(term1 >= 1, "a prepare that raised nothing granted nothing");
        let t = frame_read(&fab, prep7, 4096).unwrap();
        let term2 = fabric::PrepareReply::decode(&t).unwrap().term;
        assert!(term2 > term1, "every prepare raises the promise");

        // ---- LEARN: apply-if-newer, so one we are ahead of costs nothing ---------
        let learn7 = fabric::Cmd::Learn {
            page: page7,
            via: owner(),
        };
        let mut lt = vec![0u8; 4096];
        // Version 1: older than what we hold.
        fabric::LearnReq {
            reg: fabric::Reg {
                version: 1,
                ballot: 0,
            },
            from: fabric::Member::new(0).unwrap(),
            repair: false,
        }
        .encode(&mut lt)
        .unwrap();
        frame_write(&fab, learn7, &lt).expect("a learn we are ahead of is a no-op");
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), next);

        // ---- MERKLE and the cursor: healing --------------------------------------
        // Both carry registers and never page bytes, each riding one control block trailer.
        let addr7 = GlobalAddr::new(1, LWW + 7);
        let group = cfg.group(addr7.0);
        let bucket = heal::bucket_of(addr7.0);
        let groups = cfg.universe(1).unwrap().catalog.len() as u32;

        let merkle = |class| fabric::Cmd::Merkle {
            group: group_ix(group.index()),
            class,
        };
        let tree =
            fabric::MerkleReply::decode(&frame_read(&fab, merkle(Class::Small), 4096).unwrap())
                .expect("a well-formed digest vector")
                .0;
        assert_ne!(
            tree[bucket as usize], 0,
            "the digest is under the address's own bucket"
        );
        // The classes are separate trees, so the small page is not in the huge one.
        let huge_tree =
            fabric::MerkleReply::decode(&frame_read(&fab, merkle(Class::Huge), 4096).unwrap())
                .expect("a well-formed digest vector")
                .0;
        assert_eq!(
            huge_tree[bucket as usize], 0,
            "a class is not the other class"
        );
        // A group outside the catalog is a foreign topology, not an empty group.
        let no_group = fabric::Cmd::Merkle {
            group: group_ix(groups),
            class: Class::Small,
        };
        assert_eq!(
            errno(frame_read(&fab, no_group, 4096).unwrap_err()),
            libc::EOPNOTSUPP
        );

        // The filtered cursor names the page and the register GETMETA reported, and says
        // it is done in the same reply.
        let open = fabric::Cmd::SnapOpen {
            group: group_ix(group.index()),
            class: Class::Small,
            bucket: Some(fabric::Bucket::new(bucket).unwrap()),
        };
        let cursor = fabric::SnapOpenReply::decode(&frame_read(&fab, open, 4096).unwrap())
            .expect("a well-formed cursor")
            .cursor;
        let chunk_at = |seq: u8| fabric::Cmd::SnapNext {
            cursor,
            seq: fabric::Seq::wrap(seq),
        };
        let mut seen = std::collections::BTreeMap::new();
        let done = tuples(&frame_read(&fab, chunk_at(0), 4096).unwrap(), &mut seen);
        assert!(done, "one bucket of one group fits in one chunk");
        assert!(
            seen.keys().all(|a| heal::bucket_of(*a) == bucket),
            "the filter is a filter: {seen:?}"
        );
        assert_eq!(
            seen[&addr7.0].version,
            v7 + 1,
            "the cursor and the register agree"
        );
        assert_eq!(
            errno(frame_read(&fab, chunk_at(0), 4096).unwrap_err()),
            libc::EOPNOTSUPP,
            "a cursor that finished is gone, not stale"
        );
        // The filter narrows the walk without changing what the walk is over.
        let all = fabric::Cmd::SnapOpen {
            group: group_ix(group.index()),
            class: Class::Small,
            bucket: None,
        };
        let cursor = fabric::SnapOpenReply::decode(&frame_read(&fab, all, 4096).unwrap())
            .expect("a well-formed cursor")
            .cursor;
        let mut every = std::collections::BTreeMap::new();
        let mut seq = 0u8;
        while seq < 64 {
            let chunk = fabric::Cmd::SnapNext {
                cursor,
                seq: fabric::Seq::wrap(seq),
            };
            let t = frame_read(&fab, chunk, 4096).unwrap();
            if tuples(&t, &mut every) {
                break;
            }
            seq += 1;
        }
        assert!(every.len() >= seen.len() && every.contains_key(&addr7.0));

        // ---- Direction is part of the frame -------------------------------------
        // GET must be a read; writing one addresses a different frame entirely.
        assert_eq!(
            errno(frame_write(&fab, get7, &small).unwrap_err()),
            libc::EOPNOTSUPP
        );

        // ---- Forwarding: addressed elsewhere, and no link is EREMOTEIO -----------
        // `imm` names the addressee: `k + 1` is member index `k`, `0` means "you resolve
        // it". This node is index 0 of every group here, so anything else is forwarded,
        // and the config has no peers, so every forward is EREMOTEIO.
        let elsewhere = fabric::Via::new(
            fabric::To::Member(fabric::Member::new(1).unwrap()),
            fabric::Hops::ONE,
        );
        let fwd = fabric::Cmd::Get {
            page: page7,
            from: fabric::Source::Group(elsewhere),
            want: fabric::Want::Page,
        };
        assert_eq!(
            errno(frame_read(&fab, fwd, 4096).unwrap_err()),
            libc::EREMOTEIO,
            "no link to the named member is, to the originator, a placement it has wrong"
        );
        // A control read forwards the same way, and so does a write.
        let meta_fwd = fabric::Cmd::GetMeta {
            page: page7,
            from: fabric::Source::Group(elsewhere),
        };
        assert_eq!(
            errno(frame_read(&fab, meta_fwd, 4096).unwrap_err()),
            libc::EREMOTEIO
        );
        let trim_fwd = fabric::Cmd::Trim {
            page: page7,
            via: elsewhere,
        };
        assert_eq!(
            errno(frame_write(&fab, trim_fwd, &[0u8; 4096]).unwrap_err()),
            libc::EREMOTEIO
        );
        // A frame for someone else with no budget left is refused, not passed on.
        let spent = fabric::Cmd::Get {
            page: page7,
            from: fabric::Source::Group(fabric::Via::direct(fabric::To::Member(
                fabric::Member::new(1).unwrap(),
            ))),
            want: fabric::Want::Page,
        };
        assert!(spent.forwarded().is_none(), "no budget is no budget");
        assert_eq!(
            errno(frame_read(&fab, spent, 4096).unwrap_err()),
            libc::EREMOTEIO
        );
        // Addressed to us, either by name or by `imm == 0`, and served normally.
        let mine = fabric::Cmd::Get {
            page: page7,
            from: fabric::Source::Group(fabric::Via::direct(fabric::To::Member(
                fabric::Member::new(0).unwrap(),
            ))),
            want: fabric::Want::Page,
        };
        assert_eq!(frame_read(&fab, mine, 4096).unwrap(), next);
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), next);

        // ---- Cross-zone: homed elsewhere, so routed and never resolved here ------
        // Extent 4 lives in zone 2. Our slot table describes our own zone only, so
        // resolving it here would name the wrong group, and we hold no link to zone 2.
        let away = fabric::Cmd::Get {
            page: page_of(Class::Small, AWAY),
            from: fabric::Source::Group(owner()),
            want: fabric::Want::Page,
        };
        assert_eq!(
            errno(frame_read(&fab, away, 4096).unwrap_err()),
            libc::EREMOTEIO,
            "a foreign address is a placement the originator has wrong, not a hole"
        );
        {
            let mut afar = open_dev(&paths[3]);
            assert!(
                read_at(&mut afar, 0, 4096).is_err(),
                "a foreign page is not answered locally"
            );
        }

        // ---- TRIM: last, because it destroys the page it names -------------------
        let trim = fabric::Cmd::Trim {
            page: pageh,
            via: owner(),
        };
        frame_write(&fab, trim, &[0u8; 4096]).expect("trim");
        // Asked of a named member, the page is gone and says so, so a copy that still has
        // it can answer instead.
        let gone_piece = fabric::Cmd::Get {
            page: pageh,
            from: fabric::Source::Group(fabric::Via::direct(fabric::To::Member(
                fabric::Member::new(0).unwrap(),
            ))),
            want: fabric::Want::Piece { off: 0 },
        };
        assert_eq!(
            errno(frame_read(&fab, gone_piece, 4 << 20).unwrap_err()),
            libc::ENODATA,
            "a trimmed page is missing to the member that lost it, not empty"
        );
        // Asked of the owner, the answer is the group's, and the group agrees the page is
        // gone: a trimmed range reads as zeroes, which is what its own guest sees below.
        assert_eq!(
            frame_read(&fab, geth, 4 << 20).unwrap(),
            vec![0u8; 4 << 20],
            "a trimmed page the group agrees on is the zeroes its reader is owed"
        );
        // Direct, or the block layer answers this from the page cache it filled above.
        assert_eq!(
            direct_read(&paths[2], 0, 4 << 20).unwrap(),
            vec![0u8; 4 << 20]
        );
        frame_write(&fab, trim, &[0u8; 4096]).expect("trim is idempotent");

        // ---- SEAL: the shard stops accepting, and goes on serving reads ----------
        // Migration's exclusion: the source accepts nothing further, so the two-writer
        // window is empty rather than merely safe; reads keep working since a frozen copy
        // is final. The trailer names the extent by control-plane id, the only shard id
        // on the wire.
        let seal = fabric::Cmd::Seal {
            anchor: page7,
            via: owner(),
        };
        let mut st = vec![0u8; 4096];
        fabric::SealReq {
            term: term2 + 1,
            extent: 1,
        }
        .encode(&mut st)
        .unwrap();
        frame_write(&fab, seal, &st).expect("seal");
        assert_eq!(
            errno(frame_write(&fab, acc7, &cmd).unwrap_err()),
            libc::EREMOTEIO,
            "a sealed shard accepts nothing further"
        );
        assert_eq!(
            frame_read(&fab, get7, 4096).unwrap(),
            next,
            "a seal does not stop reads"
        );

        // A shard on its way somewhere forwards instead of refusing, so a client with a
        // pre-flip config still gets an answer. Zone 2's gateway is not a peer of ours, so
        // the forward fails at the link, not the gate: `EIO` is "routed and unreachable",
        // `EREMOTEIO` is "refused". The seal is redundant (the sweep seals an extent with
        // `next_zone` set of its own accord) and is here only so the assertion does not
        // race the tick.
        let going = page_of(Class::Small, GOING + 3);
        let acc_away = fabric::Cmd::Accept {
            page: going,
            via: owner(),
            put: fabric::Put::Gather,
        };
        let seal_away = fabric::Cmd::Seal {
            anchor: going,
            via: owner(),
        };
        let mut sta = vec![0u8; 4096];
        fabric::SealReq {
            term: term2 + 1,
            extent: 5,
        }
        .encode(&mut sta)
        .unwrap();
        frame_write(&fab, seal_away, &sta).expect("seal");
        assert_eq!(
            errno(frame_write(&fab, acc_away, &vec![0u8; 8192]).unwrap_err()),
            libc::EIO,
            "a sealed shard with somewhere to go forwards rather than refusing"
        );

        // ---- a corrupted 4 KiB page is refused, a corrupted 4 MiB page is not ----------
        // Page bytes are read from disk on every read, so damage can be done under a
        // running dataplane. Reads must be direct or the block layer answers from cache.
        write_at(&mut imm, 4 << 20, &huge);
        let small_addr = GlobalAddr::new(1, LWW + 1);
        let huge_addr = GlobalAddr::new(1, IMM + crate::config::HUGE_BLOCKS);
        let geo = layout::read_geometry(&dev).unwrap();
        let s = slot_of(&dev, Class::Small, small_addr).expect("small page placed");
        let h = slot_of(&dev, Class::Huge, huge_addr).expect("huge page placed");
        flip_byte(&dev, geo.slot_off(Class::Small, s) + 17);
        flip_byte(&dev, geo.slot_off(Class::Huge, h) + 17);

        assert!(
            direct_read(&paths[0], 4096, 4096).is_err(),
            "a 4 KiB page that fails its checksum must never be served"
        );
        // Deliberate: this class carries no checksum, so the damage is invisible and wrong
        // bytes come back as a successful read.
        assert_ne!(direct_read(&paths[2], 4 << 20, 4 << 20).unwrap(), huge);

        // Damage is contained: neighbours are unaffected.
        assert_eq!(direct_read(&paths[0], 0, 4096).unwrap(), small);
        assert_eq!(
            direct_read(&paths[0], 2 * 4096, 4096).unwrap(),
            pattern(2, 4096)
        );

        // A ublk device cannot be deleted while a client holds it open, so drop first.
        drop((lww, imm, fab));
        rt.shutdown().unwrap();
        let _ = std::fs::remove_file(&dev);
    }
}
