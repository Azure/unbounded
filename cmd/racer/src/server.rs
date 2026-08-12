//! Maps ublk requests onto allocator pages.
//!
//! The runtime sizes each device so one request is at most one page and never straddles
//! two, and the allocator owns placement, versioning and integrity. What is left is
//! address arithmetic and two rules: a hole reads as zeroes (the block layer's), and a
//! 4 MiB page is written whole or not at all (out-of-place placement's).
//!
//! A node exports a block device per configured device, which consumers mount, and one
//! *fabric* device per universe, which that universe's peers issue against. Both use the
//! same per-core workers and the same zero-copy path; they differ only in the LBA. On a
//! consumer device it is an offset into a concatenation of extents; on a fabric device it
//! is the request itself.

use std::time::{Duration, Instant};

use crate::alloc::{self, Allocator, GlobalAddr, Pressure, Status};
use crate::cache::{self, Cache};
use crate::config::{self, Config};
use crate::fabric::{self, Frame, Link, Part, status};
use crate::heal::{self, Heal};
use crate::layout;
use crate::metrics;
use crate::paxos::{self, Page, Paxos, Sink};
use crate::runtime::{
    self, Cfg, Configurator, Errno, Export, Handler, Op, PoolBuf, Request, sleep,
};

pub struct Server;
pub static SERVER: Server = Server;

/// Tag bit separating the two kinds of device key the runtime sees.
///
/// A consumer device is keyed by its configured id and a universe's fabric device by its
/// universe id, and both are 32-bit and independently assigned, so one bit above them
/// says which namespace an arriving request came in on. That bit is also the whole of
/// the demultiplex in [`Handler::handle`]: nothing else has to be looked up to know
/// whether a request is a mounted block device's or a peer's.
pub(crate) const FABRIC_TAG: u64 = 1 << 32;

/// The runtime device key of a universe's fabric device.
pub(crate) fn fabric_key(universe: u32) -> u64 {
    FABRIC_TAG | universe as u64
}

/// One published configuration: the allocator plus the devices it is exported through.
pub struct Dataplane {
    paxos: &'static Paxos,
    cache: &'static Cache,
    heal: &'static Heal,
    /// Universe id and its fabric device, in config order.
    universes: Vec<(u32, Export)>,
    /// Device id and the block device it is exported as, in config order.
    devices: Vec<(u32, Export)>,
}

impl Dataplane {
    fn alloc(&self) -> &'static Allocator {
        self.paxos.alloc()
    }

    /// Device id and the block device it is exported as, in config order.
    pub fn devices(&self) -> Vec<(u32, std::path::PathBuf)> {
        self.devices
            .iter()
            .map(|(id, v)| (*id, v.path().to_path_buf()))
            .collect()
    }

    /// Universe id and the device that universe's peers issue fabric frames against. The
    /// control plane publishes each through nvmet, to that universe's members and to
    /// nobody else: the partition is the set of nodes holding the namespace. They are not
    /// consumer devices and hold no bytes of their own.
    pub fn fabrics(&self) -> Vec<(u32, std::path::PathBuf)> {
        self.universes
            .iter()
            .map(|(id, v)| (*id, v.path().to_path_buf()))
            .collect()
    }

    /// Metadata blocks whose two copies both failed at startup and were taken out of
    /// service.
    pub fn quarantined(&self) -> usize {
        self.alloc().quarantined
    }
}

/// The consensus layer, and through it the allocator, for one node.
///
/// Built on the first configuration and reused by every later one: allocator geometry is
/// fixed at format time and its shards are sized for the worker count, so a reload swaps
/// the config it reads rather than rebuilding it from the device.
///
/// A host has one and it lives as long as the process; the simulator holds one per
/// simulated node, which is the only reason this is a value rather than a pair of
/// statics.
#[derive(Default)]
pub struct Node {
    paxos: std::sync::OnceLock<&'static Paxos>,
    heal: std::sync::OnceLock<&'static Heal>,
}

impl Node {
    pub fn new() -> Node {
        Node::default()
    }

    /// Build a configuration: open the store, install `cfg`, and attach a ublk device
    /// per configured device plus one fabric device per universe.
    ///
    /// Declare-or-not: the runtime keeps a device that is re-declared, tears down one
    /// that is not, and never disturbs a live registration, so adding or removing a
    /// device, a universe or a peer is just a different set of calls this time round.
    /// Re-placing an extent touches no device at all - the new config carries it.
    pub fn attach(&self, c: &Configurator, cfg: Config) -> std::io::Result<Dataplane> {
        let store = cfg.node.store.clone();
        let limit =
            runtime::Limiter::new(cfg.node.store_max_iops, cfg.node.store_max_bytes_per_sec);
        let disk = c.disk(&store, None, Some(limit))?;
        let cores = c.cores();
        // Declare everything the new config asks for *before* publishing it anywhere. A
        // failure below is a rejected config: the runtime discards the build, so the
        // allocator must still be reading the config that is actually running.
        let mut devices = Vec::new();
        for dev in cfg.devices() {
            devices.push((dev.id, c.device(dev.id as u64, dev.bytes(), dev.huge)?));
        }
        // Sparse by construction: a fabric device is an address space, not storage. One
        // per universe, because the namespace a frame arrives on is the only thing that
        // says which universe it is in.
        let mut universes = Vec::new();
        for u in cfg.universes() {
            universes.push((u.id, c.fabric(fabric_key(u.id), fabric::DEVICE_SIZE)?));
        }
        let mut links = Vec::new();
        for (universe, p) in cfg.peers() {
            links.push(Link::open(c, universe, p)?);
        }

        // Point of no return: nothing past here can fail on a reload.
        let paxos = match self.paxos.get() {
            Some(&p) => {
                p.alloc().install(cfg);
                p
            }
            None => {
                let alloc = alloc::open(&store, disk, cfg, cores)?;
                // One metric row per worker: this is the first point where the worker
                // count is settled.
                metrics::init(cores);
                // Consensus is leaked for the same reason the allocator is: a hop closure
                // must be `'static`, and both live as long as the process anyway.
                let cache = cache::open(alloc, cores);
                // The allocator calls loans of free 4 MiB slots back through this, from
                // inside a reservation. Installed before any worker runs.
                alloc.attach(cache);
                let p = paxos::open(alloc, cache, cores);
                let _ = self.paxos.set(p);
                let _ = self.heal.set(heal::open(p, cores));
                p
            }
        };
        paxos.install_links(links);
        // The cohort roster is derived from each universe's catalog, so it is re-derived
        // beside the allocator's own config swap.
        paxos.cache().install(paxos.alloc().config());
        let heal = *self.heal.get().expect("set beside paxos");
        Ok(Dataplane {
            paxos,
            cache: paxos.cache(),
            heal,
            universes,
            devices,
        })
    }
}

impl Handler for Server {
    type Config = Dataplane;

    async fn handle(&'static self, cfg: Cfg<Dataplane>, req: Request) -> Result<(), Errno> {
        if req.dev & FABRIC_TAG != 0 {
            // Boxed: this future is much larger than a consumer request's, and every
            // worker preallocates one slot per tag sized for the larger of the two.
            let universe = req.dev as u32;
            return Box::pin(dispatch(&cfg, universe, req)).await;
        }
        serve(&cfg, req).await.map_err(Status::errno)
    }

    fn tick(&'static self, cfg: Cfg<Dataplane>, now: Instant) {
        cfg.alloc().tick(now);
        // Sketch decay. A parked worker takes no ticks, so it is halved by elapsed time
        // rather than once per tick.
        cfg.cache.tick(now);
        // The sweep is asynchronous and `tick` is not, so this only decides whether to
        // start one; the job runs as a detached task on this core.
        cfg.heal.tick(now);
        sample(&cfg);
    }
}

/// Publish this core's counters. Every worker owns one row and writes only its own, so
/// a scrape is a sum and never a lock. Node-wide values come from core 0 alone, or the
/// sum would multiply them by the worker count.
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
    // A pressure level is not summable, so each core contributes a vote and the series
    // counts how many cores are in that state.
    match a.pressure() {
        Pressure::Low => s.alloc_pressure_low = 1,
        Pressure::Critical => s.alloc_pressure_critical = 1,
        Pressure::Normal => {}
    }
    if core == 0 {
        let cfg = a.config();
        s.alloc_quarantined = a.quarantined as u64;
        // A reload can add an extent the formatted store has no slots for. Growing needs
        // a restart, so until then the shortfall is a number to alert on rather than a
        // silent ENOSPC once the free lists run down.
        s.alloc_unbacked = layout::shortfall(&a.geometry(), cfg);
        // The tail and the part of it nobody holds. Both are node-wide, and the second
        // is the one to watch: a large gap means the store has cache space that
        // `policy.cache_index_bytes` will not pay to index.
        let (tail, unused) = d.cache.tail_bytes();
        s.cache_tail_bytes = tail;
        s.cache_unused_bytes = unused;
        // Device-wide rather than per core, so only one worker reports it.
        s.store_throttle_us = a.store_waited_us();
        s.config_generation = cfg.generation;
        s.config_rejected = config::rejected();
        // An epoch is per universe now, so the node-wide gauge is the largest one: a
        // scalar cannot carry more than that, and the per-universe value a control plane
        // needs it already knows, having published it.
        s.topology_epoch = cfg.universes().iter().map(|u| u.epoch).max().unwrap_or(0) as u64;
        s.node_id = cfg.node.id as u64;
        s.workers = a.cores() as u64;
        s.universes = cfg.universes().len() as u64;
        s.devices = cfg.devices().len() as u64;
        s.extents = cfg.extent_count() as u64;
        s.peers = cfg.peer_count() as u64;
    }
    metrics::publish(core, &s);
    // Per extent, and outside `Sample` because the rows exist only for extents a
    // configuration names. Every named extent gets a row even when this core holds
    // nothing for it: the control plane gates an epoch advance on a zero, and a series
    // that never appeared is not one.
    let cfg = a.config();
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

/// The consumer path. Every mutation is a guarded accept and every read is a quorum
/// read; consensus owns the guard, the ballot and the fan-out, leaving address
/// arithmetic and the two rules.
async fn serve(d: &Dataplane, req: Request) -> Result<(), Status> {
    let a = d.alloc();
    let px = d.paxos;
    let cfg = a.config();
    let dev = cfg.device(req.dev as u32).ok_or(Status::Unmapped)?;
    let huge = dev.huge;
    // A device is a concatenation of whole extents, so the page it hands us is a
    // position in that concatenation and this is where it becomes an address. Nothing
    // else in the stack knows the order the extents were mounted in, which is what lets
    // two hosts mount the same extents differently.
    let page = if huge { req.lba / 1024 } else { req.lba };
    let addr = GlobalAddr(dev.map(page).ok_or(Status::Unmapped)?);

    // At the low watermark we slow completions instead of failing. The tag stays
    // outstanding, so blk-mq's queue depth bounds what the kernel hands us next.
    if req.op == Op::Write && a.pressure() == Pressure::Low {
        sleep(Duration::from_micros(200)).await;
    }

    if huge {
        let off = (req.lba % 1024) as usize * 4096;
        match req.op {
            Op::Read => huge_read(d, addr, off, req).await,
            Op::Write => {
                // Out-of-place placement would make a partial write read the rest of
                // the page back first, and this class does not do that.
                if off != 0 || req.buf.len() as u64 != layout::HUGE_PAGE {
                    return Err(Status::Unmapped);
                }
                px.write(addr, Page::Huge(req.buf)).await.map(|_| ())
            }
            Op::Discard => px.trim(addr).await,
        }
    } else {
        match req.op {
            // Small pages are staged through our own registered memory, so the page
            // checksum covers bytes nobody else can change.
            Op::Read => {
                let mut page = PoolBuf::alloc(4096).await;
                match px.read(addr, Sink::Small(&mut page)).await {
                    Ok(_) => {}
                    Err(Status::Hole) => page.fill(0),
                    Err(e) => return Err(e),
                }
                req.store(0, &page).map_err(|_| Status::Io)
            }
            Op::Write => {
                let mut page = PoolBuf::alloc(4096).await;
                req.load(0, &mut page).map_err(|_| Status::Io)?;
                px.write(addr, Page::Small(&page)).await.map(|_| ())
            }
            Op::Discard => px.trim(addr).await,
        }
    }
}

/// The 4 MiB read path.
///
/// The class is Immutable-only (config validation enforces it) and a Live immutable page
/// is terminal within its epoch, so it needs no round: a miss is served locally where it
/// can be, which is also the only way a partial read can be served at all — a `GET` names
/// a page, not a byte range within one.
///
/// A cache hit is the exception: a cached copy is a claim about a version, not a value
/// this node owns, so it takes a confirming round. The round runs beside the cached read
/// rather than after it, so a hit still costs one round trip and no 4 MiB page crosses
/// the wire from the owning group.
async fn huge_read(
    d: &Dataplane,
    addr: GlobalAddr,
    off: usize,
    req: Request,
) -> Result<(), Status> {
    let a = d.alloc();
    let px = d.paxos;
    // A whole page is the only thing the cache can move between nodes for this class: a
    // `GET` addresses a page and there is no trailer to carry an offset, so a partial
    // read can only come out of our own region.
    let whole = off == 0 && req.buf.len() as u64 == layout::HUGE_PAGE;
    let w = if whole { px.cache_width(addr).await } else { 0 };
    if px.cached_huge(addr, off, w, req.buf).await {
        return Ok(());
    }
    let r = match a.read_huge(addr, off, req.buf).await {
        // We are not an acceptor for this page, so a local miss says nothing about
        // whether it exists: the bytes live in the group and repair would only heal
        // the members. A partial read cannot take this path — a `GET` names a page
        // rather than a range within one.
        Err(Status::Hole | Status::Missing) if whole && !px.member_of(addr) => {
            match px.pull_huge(addr, req.buf).await {
                Err(Status::Hole) => return a.read_zeroes(req.buf).await,
                r => r?,
            }
        }
        Err(Status::Hole | Status::Missing) if px.healable(addr) => {
            px.repair(addr).await?;
            match a.read_huge(addr, off, req.buf).await {
                Err(Status::Hole) => return a.read_zeroes(req.buf).await,
                r => r?,
            }
        }
        // A hole reads as zeroes, and we may not touch the guest's pages, so the
        // zeroes come off the device's format-time zero region.
        Err(Status::Hole) => return a.read_zeroes(req.buf).await,
        r => r?,
    };
    if whole {
        px.offer_huge(addr, w, req.buf, r.version).await;
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// fabric target
// ---------------------------------------------------------------------------

/// Serve one fabric frame.
///
/// The LBA is the request, so decoding it is the whole of parsing, and it happens before
/// anything else touches allocator state. `Frame::decode` is total, so a peer cannot
/// reach a panic from here; a frame we do not understand is a status, not a fault.
///
/// The kernel maps an initiator's submission from core *i* onto hardware queue *i*, but
/// the target's queue-to-core mapping is nvmet's business, so an arriving frame generally
/// does not land on the core owning its consensus group. The allocator's own hop fixes
/// that — it shards its index by the group hash — so it is paid below, not repeated here.
async fn dispatch(d: &Dataplane, universe: u32, req: Request) -> Result<(), Errno> {
    let (f, part) = Frame::decode(req.lba, req.buf.len())?;

    // The block layer will not hand us a write where the opcode wants a read, or the
    // reverse; a peer that gets the direction wrong has built a frame for a different
    // address than it meant to.
    if f.op.is_read() != (req.op == Op::Read) {
        return Err(status::BAD);
    }

    // Whether a frame is ours to answer is decided before the opcode is. A page op names
    // its addressee in `imm`; everything else is addressed to this node by the sender's
    // choice of link, and is ours by construction.
    if let Some(addr) = routed(d, universe, f)?
        && !d.paxos.serves(f.op, addr, f.imm)
    {
        return relay(d, f, addr, req).await;
    }

    match f.op {
        fabric::Op::Get => get(d, universe, f, part, req).await,
        fabric::Op::Trim => trim(d, universe, f, part, req).await,
        fabric::Op::Ping => ping(d, universe, part, req).await,
        fabric::Op::Accept => accept(d, universe, f, part, req).await,
        fabric::Op::GetMeta => get_meta(d, universe, f, part, req).await,
        fabric::Op::Prepare => prepare(d, universe, f, part, req).await,
        fabric::Op::Learn => learn(d, universe, f, part, req).await,
        fabric::Op::Seal => seal(d, universe, part, req).await,
        fabric::Op::Merkle => merkle(d, universe, f, part, req).await,
        fabric::Op::SnapOpen => snap_open(d, universe, f, part, req).await,
        fabric::Op::SnapNext => snap_next(d, universe, f, part, req).await,
        fabric::Op::Term => term(d, universe, f, part, req).await,
    }
}

/// The page a frame is addressed to, if it is one this node might have to forward.
///
/// Group ops name a group rather than a page and arrive by the sender's choice of link,
/// so they are always ours. `CACHE_ONLY` is excluded for the same reason: it asks
/// whoever holds a cached copy, which is a cohort replica and usually not a member of
/// the group at all.
fn routed(d: &Dataplane, universe: u32, f: Frame) -> Result<Option<GlobalAddr>, Errno> {
    use fabric::Op::*;
    match f.op {
        Get | Trim | Accept | GetMeta | Prepare | Learn | Seal
            if f.flags & fabric::CACHE_ONLY == 0 =>
        {
            Ok(Some(addr_of(d, universe, f)?))
        }
        _ => Ok(None),
    }
}

/// Frame address to allocator address.
///
/// The universe is the namespace the frame arrived on and never anything the sender
/// wrote, so this is where partitioning becomes an address: a peer given one universe's
/// namespace cannot name a page in another however it builds its frame.
///
/// Within the universe the extent lookup is the bounds check. An address no extent
/// covers is a bad frame, not a misrouted one - routing happens above this - and so is
/// one whose page class disagrees with the extent's, which would otherwise read a 4 MiB
/// page as a 4 KiB one.
fn addr_of(d: &Dataplane, universe: u32, f: Frame) -> Result<GlobalAddr, Errno> {
    let addr = config::addr_of(universe, f.lba());
    let e = d.alloc().config().extent_at(addr).ok_or(status::BAD)?;
    if e.huge != f.huge {
        return Err(status::BAD);
    }
    Ok(GlobalAddr(addr))
}

/// The consensus group a group-addressed frame names, checked against the universe it
/// arrived on.
///
/// The wire carries only an index into that universe's catalog, so a group id cannot be
/// forged across the boundary: the universe half is supplied here from the namespace.
/// An index past the catalog is a bad frame.
fn group_of(d: &Dataplane, universe: u32, index: u32) -> Result<config::GroupId, Errno> {
    let u = d.alloc().config().universe(universe).ok_or(status::BAD)?;
    if index as usize >= u.catalog.len() {
        return Err(status::BAD);
    }
    Ok(config::GroupId::new(universe, index))
}

/// Allocator outcome to fabric status.
///
/// `Conflict` covers both a CAS conflict and an Immutable page that is already written.
/// The distinction is not on the wire — only four statuses survive nvmet — and would not
/// help the caller: either way it lost a race and should look again.
fn wire(s: Status) -> Errno {
    match s {
        Status::Hole | Status::Missing => status::MISSING,
        Status::Conflict { .. } => status::STALE,
        Status::Unmapped => status::BAD,
        Status::NoSpace => status::NOSPC,
        Status::Io => Errno::EIO,
    }
}

/// `GET`: the reply *is* the payload.
async fn get(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    let a = d.alloc();
    let addr = addr_of(d, universe, f)?;
    // `CACHE_ONLY` asks for our *cached* copy and nothing else. It never reaches the
    // allocator, so answering costs the group nothing and declining costs the reader
    // one ordinary `GET` at the group.
    if f.flags & fabric::CACHE_ONLY != 0 {
        return cache_get(d, f, part, addr, req).await;
    }
    // `imm == 0` is a reader that could not name a member — it resolved our zone and
    // left the group to us — so it wants the group's answer, not this member's copy.
    // Only the 4 KiB class: the 4 MiB one is Immutable-only and takes no round at all,
    // and it arrives MDTS-split, which a round could not answer anyway.
    if f.imm == 0 && !f.huge {
        return get_confirmed(d, part, addr, req).await;
    }
    if f.huge {
        let Part::Payload { off } = part else {
            return Err(status::BAD);
        };
        // Unlike the consumer path, a hole is not zeroes here. A consumer reading an
        // unwritten page must see zeroes because the block layer says so; a peer is
        // asking group state a question, and the honest answer is that this member does
        // not have the page. Consensus then heals from another.
        return match a.read_huge(addr, off, req.buf).await {
            Err(Status::Hole) => Err(status::MISSING),
            r => r.map(|_| ()).map_err(wire),
        };
    }
    if !matches!(part, Part::Payload { off: 0 } | Part::Both) {
        return Err(status::BAD);
    }
    let mut page = PoolBuf::alloc(4096).await;
    let reg = a.read_small(addr, &mut page).await.map_err(wire)?;
    req.store(0, &page)?;
    if part == Part::Both {
        // A gathered reply carries the page's register, so the reader's hedged read
        // needs no separate `GETMETA` at this member, and the cache's width hint beside
        // it costs one byte and no command.
        d.paxos.gathered(addr, reg, &mut page).await.map_err(wire)?;
        req.store(4096, &page)?;
    }
    Ok(())
}

/// `GET` with `imm == 0`: answer for the group rather than for ourselves.
///
/// A reader in another zone resolves only the zone and so cannot fan a hedged read out
/// itself. Running the round here costs it one round trip instead of three and keeps the
/// metadata legs inside the zone that owns the page.
async fn get_confirmed(
    d: &Dataplane,
    part: Part,
    addr: GlobalAddr,
    req: Request,
) -> Result<(), Errno> {
    if !matches!(part, Part::Payload { off: 0 } | Part::Both) {
        return Err(status::BAD);
    }
    let mut page = PoolBuf::alloc(4096).await;
    let r = d
        .paxos
        .read_for(addr, Sink::Small(&mut page))
        .await
        .map_err(wire)?;
    req.store(0, &page)?;
    if part == Part::Both {
        // The round already confirmed this register, so the reader needs no metadata leg
        // of its own. No width: the hint belongs to whoever owns the sketch.
        let mut t = [0u8; fabric::BLOCK];
        paxos::put_register(&mut t, r, 0);
        req.store(4096, &t)?;
    }
    Ok(())
}

/// `GET` under [`fabric::CACHE_ONLY`].
///
/// The reply is whatever this node happens to be holding as a cohort replica. There is
/// no fallback here and there must not be: the point of the flag is that a miss is cheap
/// and lands back at the reader, which then asks the group.
async fn cache_get(
    d: &Dataplane,
    f: Frame,
    part: Part,
    addr: GlobalAddr,
    req: Request,
) -> Result<(), Errno> {
    // A shedding replica owes the reader an `EAGAIN`. Only four errnos survive nvmet, so
    // it answers `MISSING` instead — indistinguishable from a miss on the wire, and the
    // reader's fallback is the same either way. Shedding is always safe: the cache has
    // no correctness role at all.
    if d.cache.shedding() {
        return Err(status::MISSING);
    }
    if f.huge {
        let Part::Payload { off } = part else {
            return Err(status::BAD);
        };
        // The reader confirms what we hand back against the group, so we need not; but
        // we owe it a single entry, so the version filter here is the one the paired
        // `GETMETA` applies and the two cannot name different fills of the page.
        return match d.cache.load_immutable(addr, true, off, req.buf).await {
            Some(_) => Ok(()),
            None => Err(status::MISSING),
        };
    }
    if part != Part::Both {
        return Err(status::BAD);
    }
    let mut page = PoolBuf::alloc(2 * fabric::BLOCK).await;
    let buf = page.buf().slice(0, 4096);
    let r = d
        .cache
        .load(addr, false, 0, buf)
        .await
        .ok_or(status::MISSING)?;
    // The register the entry claims travels with it; the reader confirms it against the
    // quorum and drops the entry if it does not match. No width: we are a replica here,
    // not the owner, and have no business advertising one.
    paxos::put_register(&mut page[fabric::BLOCK..], r, 0);
    req.store(0, &page)
}

/// `TRIM`: delete a page. A guarded accept whose value is a tombstone, so a repeat says
/// nothing new.
async fn trim(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let addr = addr_of(d, universe, f)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    req.load(0, &mut t)?;
    d.paxos.accept_trim(addr, f.imm, &t).await.map_err(wire)
}

/// `ACCEPT`: apply a page under a ballot. A 4 KiB frame gathers its guard and ballot
/// into a trailer beside the page; a 4 MiB frame is all payload and has neither, so the
/// acceptor derives both — legal only because the 4 MiB class is Immutable, whose guard
/// every replica computes from the config alone.
async fn accept(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    let addr = addr_of(d, universe, f)?;
    if !paxos::accept_parts(f.huge, part) {
        return Err(status::BAD);
    }
    if f.huge {
        let Part::Payload { off } = part else {
            return Err(status::BAD);
        };
        // A transport splits a 4 MiB command at its MDTS, so the page can arrive as
        // consecutive pieces. One whole page still takes the path it always did.
        if off == 0 && req.buf.len() as u64 == layout::HUGE_PAGE {
            return d
                .paxos
                .accept(addr, f.imm, None, Page::Huge(req.buf))
                .await
                .map_err(wire);
        }
        return d
            .paxos
            .accept_part(addr, f.imm, off as u32, req.buf)
            .await
            .map_err(wire);
    }
    let mut both = PoolBuf::alloc(2 * fabric::BLOCK).await;
    req.load(0, &mut both)?;
    let (page, trailer) = both.split_at(fabric::BLOCK);
    // Staged into our own memory so the checksum covers bytes nobody else can change,
    // as the consumer path does.
    let mut p = PoolBuf::alloc(fabric::BLOCK).await;
    p.copy_from_slice(page);
    let t = trailer.to_vec();
    d.paxos
        .accept(addr, f.imm, Some(&t), Page::Small(&p))
        .await
        .map_err(wire)
}

/// `GETMETA`: the metadata half of a hedged read. No page moves, and the answer is a
/// register the owning core already holds.
///
/// Under [`fabric::CACHE_ONLY`] it answers for our *cached* copy instead: the register
/// half of a 4 MiB cache hit, whose class has no trailer to gather one into, so it rides
/// a second command beside the page.
async fn get_meta(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let addr = addr_of(d, universe, f)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    if f.flags & fabric::CACHE_ONLY != 0 {
        if d.cache.shedding() {
            return Err(status::MISSING);
        }
        let r = d
            .cache
            .peek_immutable(addr, f.huge)
            .await
            .ok_or(status::MISSING)?;
        // No width: we are a replica here, not the owner.
        paxos::put_register(&mut t, r, 0);
        return req.store(0, &t);
    }
    d.paxos.get_meta(addr, &mut t).await.map_err(wire)?;
    req.store(0, &t)
}

/// `PREPARE`: raise this group's promise and report what we hold. A read carries no
/// request body, so the term is not on the wire: we raise our own by one and the
/// preparer takes the maximum it hears back.
async fn prepare(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let addr = addr_of(d, universe, f)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    d.paxos.prepare(addr, &mut t).await.map_err(wire)?;
    req.store(0, &t)
}

/// `LEARN`: a value we may be behind on, and the member holding it. Apply-if-newer, so
/// a repeat is free and the repair and migration streams commute.
async fn learn(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let addr = addr_of(d, universe, f)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    req.load(0, &mut t)?;
    let (r, from, repair) = paxos::learn_trailer(&t);
    d.paxos.learn(addr, r, from, repair).await.map_err(wire)
}

/// `SEAL`: freeze a shard at its source group. An ordinary accept whose value happens
/// to be a shard rather than a page.
async fn seal(d: &Dataplane, universe: u32, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    req.load(0, &mut t)?;
    let (extent, term) = paxos::seal_trailer(&t);
    // An extent id is unique across every universe, so it names the shard on its own;
    // that also means the id alone would let a peer seal a shard outside its own
    // partition, and this is the check that stops it.
    let ours = d
        .alloc()
        .config()
        .extent_by_id(extent)
        .is_some_and(|(u, _)| u.id == universe);
    if !ours {
        return Err(status::BAD);
    }
    d.paxos.seal(extent, term).await.map_err(wire)
}

/// The three anti-entropy ops. Their frames name a *consensus group*, not a page, so
/// `addr_of` is deliberately off these paths and `dev`/`offset` mean what `heal.rs` says
/// they mean rather than what the frame layout says.
///
/// `MERKLE`: our digest vector for one group and class. A group we hold nothing for
/// answers all zeroes rather than an error — the correct digest of an empty set, which a
/// peer that also holds nothing has to agree with.
async fn merkle(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let group = group_of(d, universe, f.offset as u32)?;
    let v = d.alloc().digests(group, f.imm & 1 == 1).await;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    heal::put_digests(&mut t, &v);
    req.store(0, &t)
}

/// `SNAPOPEN`: begin enumerating a group's registers, optionally filtered to one digest
/// bucket. Reply slot 0 is the cursor id, which is self-describing so `SNAPNEXT` needs
/// nothing else. `NOSPC` means this slab already holds as many cursors as it will.
async fn snap_open(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let (index, huge, bucket) = heal::snap_open_parts(&f);
    let group = group_of(d, universe, index)?;
    let id = d
        .alloc()
        .snap_open(
            group,
            huge,
            bucket.map_or(heal::Filter::All, heal::Filter::Bucket),
        )
        .await
        .map_err(wire)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    t.fill(0);
    fabric::put(&mut t, 0, id as u64);
    req.store(0, &t)
}

/// `SNAPNEXT`: the next chunk of `(address, version, ballot)`, and no page bytes. The
/// sequence number in the frame makes a retry idempotent, which matters because a cursor
/// that skipped a chunk would silently under-report a difference.
///
/// The last chunk closes the cursor: there is no explicit close on the wire and a
/// finished cursor holds reclamation for nothing. So a lost final reply is not retryable
/// — the reader sees a bad frame and repeats the bucket next pass.
async fn snap_next(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let (id, seq) = heal::snap_next_parts(&f);
    // A cursor id is a small integer, so the universe it was opened in is checked
    // against the one asking rather than trusted from the frame.
    let (tuples, done) = d
        .alloc()
        .snap_next(id, Some(seq), Some(universe))
        .await
        .map_err(wire)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    t.fill(0);
    heal::put_tuples(&mut t, &tuples, done);
    if done {
        d.alloc().snap_release(id).await;
    }
    req.store(0, &t)
}

/// `TERM`: the promise we hold for one group, for a member rebuilding its own. Trailer
/// slot 2. A group we hold nothing for answers zero, which cannot lower the maximum the
/// caller is taking.
async fn term(
    d: &Dataplane,
    universe: u32,
    f: Frame,
    part: Part,
    req: Request,
) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let group = group_of(d, universe, f.offset as u32)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    d.paxos.term(group, &mut t).await.map_err(wire)?;
    req.store(0, &t)
}

/// `PING`: liveness, and the geometry a caller needs to tell a stale answer from a
/// current one. Trailer slots: 0 node id, 1 config generation, 2 topology epoch.
///
/// The epoch is the arriving universe's: a caller in one universe has no business
/// learning another's, and would not know what to do with it if it did.
async fn ping(d: &Dataplane, universe: u32, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let mut t = PoolBuf::alloc(4096).await;
    t.fill(0);
    let c = d.alloc().config();
    fabric::put(&mut t, 0, c.node.id as u64);
    fabric::put(&mut t, 1, c.generation);
    let epoch = c.universe(universe).map_or(0, |u| u.epoch);
    fabric::put(&mut t, 2, epoch as u64);
    req.store(0, &t)
}

/// A frame that is not ours: reissue it on our own link and complete when it does.
///
/// We hold only the in-flight future — no session, no buffering, no copy. The frame keeps
/// its shape, so the same registered buffer serves both hops: a forwarded read DMAs the
/// page back through it, a forwarded write pushes the page forward through it. If we die
/// the outer command fails, which the originator reads as a timeout.
///
/// The budget terminates the recursion: each hop spends one, and a frame that is not ours
/// with none left is refused. That also bounds a migration forward: one that would chain
/// past the budget sends the originator back to its config.
///
/// A relay never leaves the universe: the link it picks is one of that universe's, so a
/// forwarded frame is still inside the partition it arrived in.
async fn relay(d: &Dataplane, f: Frame, addr: GlobalAddr, req: Request) -> Result<(), Errno> {
    // No budget, or a member we hold no link to: either way the originator has our
    // placement wrong, so it is sent back to its config rather than told the page is
    // missing.
    if fabric::hops(f.flags) == 0 {
        return Err(status::STALE);
    }
    let link = d
        .paxos
        .forward_link(f.op, addr, f.imm)
        .ok_or(status::STALE)?;
    link.send(f.forwarded(), req.buf).await
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

/// The whole stack through a real ublk block device: format, serve, write, read,
/// discard, most fabric frames (`TERM` is not exercised), and the corruption asymmetry
/// as seen by the block layer.
///
/// In the crate rather than in `tests/` so the allocator can stay private. Needs root,
/// and needs a working ublk subsystem: it creates and destroys devices.
///
/// One dataplane, one process, one boot: a restart is a real process restart or it is
/// nothing, and `tests/cluster.rs` restarts a real process.
#[cfg(test)]
mod tests {
    use std::io::{Read, Seek, SeekFrom, Write};
    use std::os::fd::AsRawFd;
    use std::path::{Path, PathBuf};
    use std::sync::{Arc, Mutex};

    use super::{self as server, SERVER};
    use crate::alloc::GlobalAddr;
    use crate::config::Config;
    use crate::fabric;
    use crate::heal;
    use crate::layout::{self, Class, State};
    use crate::runtime;

    const IMG: &str = "racer-e2e-alloc.img";
    const DEV_BYTES: u64 = 1 << 30;

    /// One universe, laid out by hand: extent 1 is LWW, extent 2 is OCC, extent 3 is
    /// immutable 4 MiB. Eight groups so addresses spread over the workers and the
    /// cross-core paths are used.
    ///
    /// Extent 4 is homed in zone 2, which this node is not in and holds no link to, so
    /// routing a foreign address is exercised rather than resolved. Extent 5 is homed
    /// here but on its way to zone 2, so it serves locally until it is sealed and
    /// forwards afterwards.
    ///
    /// The gap between extent 2 and extent 3 is deliberate: an LBA in it belongs to no
    /// extent, which is what a frame addressing past the end of anything has to hit.
    /// Extent 3 starts 1024-aligned because a 4 MiB page covers 1024 blocks.
    ///
    /// Devices 1 to 5 each export one extent. Device 6 exports extents 2 and 1, in that
    /// order: a device is an arbitrary ordered set of extents, so the same extent may be
    /// mounted twice, at a different offset, and the address it resolves to is the
    /// extent's, not the device's.
    ///
    /// This node is a member of every group. It has to be: consensus routes a page to
    /// the group that owns it, and with no peers declared a group we are not in has no
    /// reachable member at all. A single node is member index 0 everywhere, so its
    /// quorum is one and a local accept is a decision. Being in every group also pins
    /// the zone to three nodes, since an equal share of eight groups over anything wider
    /// would leave us holding more of the zone than the rest.
    fn config_text(dev: &Path) -> String {
        format!(
            "
            generation 1
            node id=1 zone=1 store={} size={DEV_BYTES}
            universe 1
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            zone id=2 entry=2,3,4
            extent id=1 base=0 pages=4096 kind=lww zone=1
            extent id=2 base=4096 pages=256 kind=occ zone=1
            extent id=3 base=5120 pages=2 kind=immutable_4m zone=1
            extent id=4 base=7168 pages=64 kind=lww zone=2
            extent id=5 base=7232 pages=64 kind=lww zone=1 next_zone=2
            device 1 extents=1
            device 2 extents=2
            device 3 extents=3
            device 4 extents=4
            device 5 extents=5
            device 6 extents=2,1
            ",
            dev.display()
        )
    }

    /// The precondition is not "am I root" but "can the runtime bring a ublk device up":
    /// a kernel built without `ublk_drv` refuses these tests however privileged we are,
    /// so probe the control node itself.
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

    /// Where the allocator put a page, read straight out of the metadata region. Used to
    /// corrupt a specific page's bytes behind the allocator's back.
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

    /// A frame issued against our own fabric device, which is what a peer's link would
    /// do: the peer's namespace *is* this block device, so driving it locally exercises
    /// every line the remote path would except the transport itself.
    fn frame_read(f: &layout::Dev, fr: fabric::Frame, len: usize) -> std::io::Result<Vec<u8>> {
        let mut buf = layout::Aligned::new(len);
        layout::read_at(f, buf.as_mut(), fr.encode() * 4096)?;
        Ok(buf.as_ref().to_vec())
    }

    fn frame_write(f: &layout::Dev, fr: fabric::Frame, data: &[u8]) -> std::io::Result<()> {
        let mut buf = layout::Aligned::new(data.len());
        buf.as_mut().copy_from_slice(data);
        layout::write_at(f, buf.as_ref(), fr.encode() * 4096)
    }

    fn errno(e: std::io::Error) -> i32 {
        e.raw_os_error().unwrap_or(0)
    }

    /// A read that has to reach the device. The block layer would otherwise answer a
    /// buffered read from bytes it fetched before we corrupted the disk underneath it.
    fn direct_read(p: &Path, off: u64, len: usize) -> std::io::Result<Vec<u8>> {
        let f = layout::open_direct(p, false)?;
        let mut b = layout::Aligned::new(len);
        layout::read_at(&f, b.as_mut(), off)?;
        Ok(b.as_ref().to_vec())
    }

    /// One boot, three layers: what a block client sees, what a peer sees over the
    /// fabric, and what a disk that has gone wrong looks like from both. Needs the real
    /// kernel seams, which `sim` replaces.
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

        // Discard is a trim, and a trimmed page is one the extent no longer has: to a
        // block client that is indistinguishable from one never written.
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
        // Device 6 is extents 2 and 1 concatenated, so its page 256 is extent 1's page
        // 0: the same bytes as device 1's page 0, reached at a different offset through
        // a different device. An address belongs to an extent, and a device is only an
        // order the control plane put some extents in.
        let mut both_ext = open_dev(&paths[5]);
        assert_eq!(read_at(&mut both_ext, 0, 4096).unwrap(), vec![0u8; 4096]);
        assert_eq!(
            read_at(&mut both_ext, (256 + 2) * 4096, 4096).unwrap(),
            pattern(2, 4096)
        );
        // And it is one page, not two copies of one: a write through the second mapping
        // is visible through the first.
        let shared = pattern(0xc3, 4096);
        write_at(&mut both_ext, (256 + 8) * 4096, &shared);
        assert_eq!(direct_read(&paths[0], 8 * 4096, 4096).unwrap(), shared);
        drop(both_ext);

        // ---- the fabric, against itself -----------------------------------------
        // The node's own fabric device, opened as if it were a peer's. Everything but
        // the two nvme hops is real — the same decode, dispatch and allocator. There is
        // no nvmet here, but nvme-of is transparent by construction: the peer's
        // namespace *is* this block device.
        //
        // The one thing loopback cannot check is the status alphabet, and it fails in
        // the *permissive* direction, so the assertions below are deliberately narrow.
        // A loopback error goes `ublk -> BLK_STS_* -> errno` and keeps roughly a dozen
        // values; a real one goes on through `nvmet -> NVMe -> initiator` and keeps
        // four. So a code this test observes is not necessarily one a peer would
        // observe, and only the four in `fabric::status` may be asserted on.
        //
        // A frame carries no extent and no device, only a block offset in the
        // universe's own LBA space: the namespace it arrives on is the universe, and
        // which extent covers that offset is the control plane's business.
        const LWW: u64 = 0; // extent 1
        const IMM: u64 = 5120; // extent 3, 1024-aligned because it is 4 MiB
        const AWAY: u64 = 7168; // extent 4, zone 2
        const GOING: u64 = 7232; // extent 5, zone 1 -> zone 2
        const GAP: u64 = 4500; // between extent 2 and extent 3: no extent at all

        // O_DIRECT so every frame is a real command: a frame is a request, not an
        // address, and a cached reply would answer a different question.
        let fab = layout::open_direct(&fab_path, true).unwrap();
        write_at(&mut lww, 7 * 4096, &small);

        // ---- GET, bare: the reply is the payload -------------------------------
        let get7 = fabric::Frame::page(fabric::Op::Get, false, LWW + 7);
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), small);

        // ---- GET, gather: page then trailer, one command ------------------------
        // The trailer is the register the page was chosen at: value and proof in one
        // command.
        let both = frame_read(&fab, get7, 8192).unwrap();
        assert_eq!(&both[..4096], &small[..]);
        let v7 = fabric::get(&both[4096..], 0);
        assert_ne!(v7, 0, "a page that exists was written at some version");
        assert_ne!(
            fabric::get(&both[4096..], 1),
            0,
            "and at the ballot that chose it"
        );

        // ---- A page this member does not have is not zeroes, it is MISSING ------
        let gone = fabric::Frame::page(fabric::Op::Get, false, LWW + 999);
        let e = frame_read(&fab, gone, 4096).unwrap_err();
        assert_eq!(
            errno(e),
            libc::ENODATA,
            "a hole is a hole, not a page of zeroes"
        );
        // The same page over the block device still reads as zeroes: that is what the
        // block layer requires of a consumer.
        assert_eq!(
            read_at(&mut lww, 999 * 4096, 4096).unwrap(),
            vec![0u8; 4096]
        );

        // ---- Addressing: unmapped block, and the wrong page class ---------------
        // The universe's address space is sparse, so an offset is answerable only where
        // an extent covers it. A gap between two extents is as unmapped as an offset
        // past every extent, and neither is a hole.
        let gap = fabric::Frame::page(fabric::Op::Get, false, GAP);
        assert_eq!(
            errno(frame_read(&fab, gap, 4096).unwrap_err()),
            libc::EOPNOTSUPP
        );
        let beyond = fabric::Frame::page(fabric::Op::Get, false, 1 << 30);
        assert_eq!(
            errno(frame_read(&fab, beyond, 4096).unwrap_err()),
            libc::EOPNOTSUPP
        );
        let miscl = fabric::Frame::page(fabric::Op::Get, true, LWW);
        assert_eq!(
            errno(frame_read(&fab, miscl, 4096).unwrap_err()),
            libc::EOPNOTSUPP
        );

        // ---- GET, 4 MiB: whole, and in the pieces an MDTS split would produce ----
        let geth = fabric::Frame::page(fabric::Op::Get, true, IMM);
        assert_eq!(frame_read(&fab, geth, 4 << 20).unwrap(), huge);
        for (block, len) in [(0usize, 256 << 10), (64, 256 << 10), (1023, 4096)] {
            let mut b = layout::Aligned::new(len);
            let off = (geth.encode() + block as u64) * 4096;
            layout::read_at(&fab, b.as_mut(), off).unwrap();
            assert_eq!(
                b.as_ref(),
                &huge[block * 4096..block * 4096 + len],
                "block {block}"
            );
        }

        // ---- PING: liveness and the geometry that dates an answer ---------------
        let ping = fabric::Frame::raw(fabric::Op::Ping, false, 0);
        let t = frame_read(&fab, ping, 4096).unwrap();
        assert_eq!(fabric::get(&t, 0), 1, "node id");
        assert_eq!(fabric::get(&t, 1), 1, "config generation");
        assert_eq!(fabric::get(&t, 2), 0, "topology epoch");
        // Control ops are exactly one block.
        assert!(frame_read(&fab, ping, 8192).is_err());

        // A small ACCEPT must gather page and trailer into one command; a page on its
        // own is a bad frame.
        let acc7 = fabric::Frame::page(fabric::Op::Accept, false, LWW + 7);
        assert_eq!(
            errno(frame_write(&fab, acc7, &small).unwrap_err()),
            libc::EOPNOTSUPP
        );

        // ---- GETMETA: the register, and not one byte of the page -----------------
        let meta7 = fabric::Frame::page(fabric::Op::GetMeta, false, LWW + 7);
        let t = frame_read(&fab, meta7, 4096).unwrap();
        assert_eq!(
            fabric::get(&t, 0),
            v7,
            "the metadata read and the gathered trailer agree"
        );
        assert_ne!(
            fabric::get(&t, 1),
            0,
            "a chosen value carries the ballot that chose it"
        );

        // ---- ACCEPT: one round trip, and the guard is the collision detector ------
        // The page and the trailer travel together, so a whole proposal is one command:
        // `imm` zero says the sender is not a member and we are the proposer.
        let next = pattern(0x11, 4096);
        let mut cmd = vec![0u8; 8192];
        cmd[..4096].copy_from_slice(&next);
        fabric::put(&mut cmd[4096..], 0, v7);
        frame_write(&fab, acc7, &cmd).expect("an accept at the version we hold");
        // Read back over the fabric, not the block device: that fd is buffered and
        // would answer from its own cache without asking us.
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), next);

        // The same guard a second time is a proposal that lost its race: refused rather
        // than reordered, which is the whole of the acceptance rule.
        assert_eq!(
            errno(frame_write(&fab, acc7, &cmd).unwrap_err()),
            libc::EREMOTEIO,
            "a guard that no longer matches is refused, not applied"
        );
        let t = frame_read(&fab, meta7, 4096).unwrap();
        assert_eq!(fabric::get(&t, 0), v7 + 1, "the loser left no trace");

        // ---- PREPARE: raises this group's promise and reports the register -------
        let prep7 = fabric::Frame::page(fabric::Op::Prepare, false, LWW + 7);
        let t = frame_read(&fab, prep7, 4096).unwrap();
        assert_eq!(
            fabric::get(&t, 0),
            v7 + 1,
            "prepare reports, it does not write"
        );
        let term1 = fabric::get(&t, 2);
        assert!(term1 >= 1, "a prepare that raised nothing granted nothing");
        let t = frame_read(&fab, prep7, 4096).unwrap();
        let term2 = fabric::get(&t, 2);
        assert!(term2 > term1, "every prepare raises the promise");

        // ---- LEARN: apply-if-newer, so one we are ahead of costs nothing ---------
        let learn7 = fabric::Frame::page(fabric::Op::Learn, false, LWW + 7);
        let mut lt = vec![0u8; 4096];
        fabric::put(&mut lt, 0, 1); // version 1: older than what we hold
        frame_write(&fab, learn7, &lt).expect("a learn we are ahead of is a no-op");
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), next);

        // ---- MERKLE and the cursor: healing --------------------------------------
        // Both carry registers and never page bytes, so a digest and a chunk of a
        // snapshot each ride the trailer of one control block.
        let addr7 = GlobalAddr::new(1, LWW + 7);
        let group = cfg.group(addr7.0);
        let bucket = heal::bucket_of(addr7.0);
        let groups = cfg.universe(1).unwrap().catalog.len() as u32;

        // The page is filed under the group and the bucket its address hashes to.
        let tree = heal::get_digests(
            &frame_read(&fab, heal::merkle_frame(group.index(), false), 4096).unwrap(),
        );
        assert_ne!(
            tree[bucket as usize], 0,
            "the digest is under the address's own bucket"
        );
        // The classes are separate trees, so the small page is not in the huge one.
        // Zeroes are the digest of the empty set, not a refusal.
        let huge_tree = heal::get_digests(
            &frame_read(&fab, heal::merkle_frame(group.index(), true), 4096).unwrap(),
        );
        assert_eq!(
            huge_tree[bucket as usize], 0,
            "a class is not the other class"
        );
        // A group outside the catalog is a frame built against a topology we do not
        // share, which is not the same as a group that happens to be empty.
        let no_group = heal::merkle_frame(groups, false);
        assert_eq!(
            errno(frame_read(&fab, no_group, 4096).unwrap_err()),
            libc::EOPNOTSUPP
        );

        // The bucket-filtered cursor names the page and the register GETMETA reported,
        // and says it is finished in the same reply.
        let open = heal::snap_open_frame(group.index(), false, Some(bucket));
        let id = fabric::get(&frame_read(&fab, open, 4096).unwrap(), 0) as u32;
        let mut seen = std::collections::BTreeMap::new();
        let done = heal::get_tuples(
            &frame_read(&fab, heal::snap_next_frame(id, 0), 4096).unwrap(),
            &mut seen,
        );
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
        // The last chunk closes the cursor: there is no close on the wire.
        assert_eq!(
            errno(frame_read(&fab, heal::snap_next_frame(id, 0), 4096).unwrap_err()),
            libc::EOPNOTSUPP,
            "a cursor that finished is gone, not stale"
        );
        // The filter narrows the walk without changing what the walk is over.
        let all = heal::snap_open_frame(group.index(), false, None);
        let id = fabric::get(&frame_read(&fab, all, 4096).unwrap(), 0) as u32;
        let mut every = std::collections::BTreeMap::new();
        let mut seq = 0u8;
        while seq < 64 {
            let t = frame_read(&fab, heal::snap_next_frame(id, seq), 4096).unwrap();
            if heal::get_tuples(&t, &mut every) {
                break;
            }
            seq += 1;
        }
        assert!(every.len() >= seen.len() && every.contains_key(&addr7.0));

        // ---- Direction is part of the frame -------------------------------------
        // GET is interrogative and so must be a read; writing one is writing to a
        // different address, not a smaller mistake.
        assert_eq!(
            errno(frame_write(&fab, get7, &small).unwrap_err()),
            libc::EOPNOTSUPP
        );

        // ---- Forwarding: addressed elsewhere, and no link is EREMOTEIO -----------
        // `imm` names the addressee — `k + 1` is member index `k`, and `0` is "you
        // resolve it". This node is index 0 of every group here, so anything else is
        // a frame to pass on, and the config has no peers, so every forward is
        // EREMOTEIO.
        let mut fwd = fabric::Frame::page(fabric::Op::Get, false, LWW + 7);
        fwd.imm = 2;
        fwd.flags = 1;
        assert_eq!(
            errno(frame_read(&fab, fwd, 4096).unwrap_err()),
            libc::EREMOTEIO,
            "no link to the named member is, to the originator, a placement it has wrong"
        );
        // A control read forwards the same way, and so does a write.
        let mut meta_fwd = fabric::Frame::page(fabric::Op::GetMeta, false, LWW + 7);
        meta_fwd.imm = 2;
        meta_fwd.flags = 1;
        assert_eq!(
            errno(frame_read(&fab, meta_fwd, 4096).unwrap_err()),
            libc::EREMOTEIO
        );
        let mut trim_fwd = fabric::Frame::page(fabric::Op::Trim, false, LWW + 7);
        trim_fwd.imm = 2;
        trim_fwd.flags = 1;
        assert_eq!(
            errno(frame_write(&fab, trim_fwd, &[0u8; 4096]).unwrap_err()),
            libc::EREMOTEIO
        );
        // A frame for someone else with no budget left is refused rather than passed
        // on: the budget terminates the recursion.
        let mut spent = fabric::Frame::page(fabric::Op::Get, false, LWW + 7);
        spent.imm = 2;
        assert_eq!(fabric::hops(spent.flags), 0);
        assert_eq!(
            errno(frame_read(&fab, spent, 4096).unwrap_err()),
            libc::EREMOTEIO
        );
        // Addressed to us — either by name or by `imm == 0` — and served normally.
        let mut mine = fabric::Frame::page(fabric::Op::Get, false, LWW + 7);
        mine.imm = 1;
        assert_eq!(frame_read(&fab, mine, 4096).unwrap(), next);
        let resolve = fabric::Frame::page(fabric::Op::Get, false, LWW + 7);
        assert_eq!(frame_read(&fab, resolve, 4096).unwrap(), next);

        // ---- Cross-zone: homed elsewhere, so routed and never resolved here ------
        // Extent 4 lives in zone 2. Our slot table describes our own zone only, so
        // resolving one of its addresses against it would name a group in the wrong
        // zone; with no link to zone 2's entry nodes there is nowhere to send it.
        let away = fabric::Frame::page(fabric::Op::Get, false, AWAY);
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
        let trim = fabric::Frame::page(fabric::Op::Trim, true, IMM);
        frame_write(&fab, trim, &[0u8; 4096]).expect("trim");
        assert_eq!(
            errno(frame_read(&fab, geth, 4 << 20).unwrap_err()),
            libc::ENODATA,
            "a trimmed page is missing, not empty"
        );
        // Idempotent: a tombstone is a state, so saying it twice says nothing new.
        frame_write(&fab, trim, &[0u8; 4096]).expect("trim is idempotent");

        // ---- SEAL: the shard stops accepting, and goes on serving reads ----------
        // Migration's exclusion: the source is driven to a state where it accepts
        // nothing further, which makes the two-writer window empty rather than merely
        // safe. Reads keep working, because a frozen copy is a final one.
        // The frame names a page; the trailer names the extent being frozen, by the id
        // the control plane gave it. Nothing else on the wire identifies a shard.
        let seal = fabric::Frame::page(fabric::Op::Seal, false, LWW + 7);
        let mut st = vec![0u8; 4096];
        fabric::put(&mut st, 1, term2 + 1);
        fabric::put(&mut st, 2, 1);
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

        // A shard on its way somewhere forwards instead of refusing, so a client holding
        // a config from before the flip still gets an answer. Zone 2's entry node is not
        // a peer of ours, so the forward fails at the link rather than at the gate —
        // that is the distinction asserted: `EIO` is "routed and unreachable",
        // `EREMOTEIO` is "refused".
        //
        // The seal is redundant — the sweep seals an extent with `next_zone` set of its
        // own accord — and is here only so the assertion does not race the tick.
        let acc_away = fabric::Frame::page(fabric::Op::Accept, false, GOING + 3);
        let seal_away = fabric::Frame::page(fabric::Op::Seal, false, GOING + 3);
        let mut sta = vec![0u8; 4096];
        fabric::put(&mut sta, 1, term2 + 1);
        fabric::put(&mut sta, 2, 5);
        frame_write(&fab, seal_away, &sta).expect("seal");
        assert_eq!(
            errno(frame_write(&fab, acc_away, &vec![0u8; 8192]).unwrap_err()),
            libc::EIO,
            "a sealed shard with somewhere to go forwards rather than refusing"
        );

        // ---- a corrupted 4 KiB page is refused, a corrupted 4 MiB page is not ----------
        // Page bytes are read from disk on every read, so damage can be done underneath
        // a running dataplane; the reads must be direct or the block layer answers them
        // out of its own cache.
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
        // Deliberate: this class carries no checksum, so the damage is invisible to us
        // and the wrong bytes come back as a successful read.
        assert_ne!(direct_read(&paths[2], 4 << 20, 4 << 20).unwrap(), huge);

        // Damage is contained: neighbours are unaffected.
        assert_eq!(direct_read(&paths[0], 0, 4096).unwrap(), small);
        assert_eq!(
            direct_read(&paths[0], 2 * 4096, 4096).unwrap(),
            pattern(2, 4096)
        );

        // A ublk device cannot be deleted while a client still holds it open, so the
        // client always lets go first.
        drop((lww, imm, fab));
        rt.shutdown().unwrap();
        let _ = std::fs::remove_file(&dev);
    }
}
