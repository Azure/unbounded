//! Maps ublk requests onto allocator pages.
//!
//! The runtime sizes each device so one request is at most one page and never straddles
//! two, and the allocator owns placement, versioning and integrity. What is left is
//! address arithmetic and two rules: a hole reads as zeroes (the block layer's), and a
//! 4 MiB page is written whole or not at all (out-of-place placement's).
//!
//! A node exports a volume per config entry, which consumers mount, and one *fabric*
//! device, which peers issue against. Both use the same per-core workers and the same
//! zero-copy path; they differ only in the LBA. On a volume it is an offset; on the
//! fabric device it is the request itself.

use std::time::{Duration, Instant};

use crate::alloc::{self, Allocator, GlobalAddr, Pressure, Status};
use crate::config::{self, Config};
use crate::cache::{self, Cache};
use crate::fabric::{self, Frame, Link, Part, status};
use crate::heal::{self, Heal};
use crate::layout;
use crate::metrics;
use crate::paxos::{self, Page, Paxos, Sink};
use crate::runtime::{self, Cfg, Configurator, Errno, Handler, Op, PoolBuf, Request, Volume, sleep};

pub struct Server;
pub static SERVER: Server = Server;

/// Device key of the fabric device. Config validation reserves volume id 0, so it
/// cannot collide with a consumer volume.
const FABRIC_KEY: u64 = 0;

/// One published configuration: the allocator plus the devices it is exported through.
pub struct Dataplane {
    paxos: &'static Paxos,
    cache: &'static Cache,
    heal: &'static Heal,
    volumes: Vec<Volume>,
    fabric: Volume,
    /// This node's share of its zone's slots. Counting it is a scan of the whole slot
    /// table and the answer only moves when a configuration is installed, so it is
    /// taken once here rather than on every metrics tick.
    share_slots: u32,
}

impl Dataplane {
    fn alloc(&self) -> &'static Allocator {
        self.paxos.alloc()
    }

    /// Volume id and the block device it is exported as, in config order.
    pub fn devices(&self) -> Vec<(u32, std::path::PathBuf)> {
        let ids = self.alloc().config().volumes.iter().map(|v| v.id);
        ids.zip(self.volumes.iter().map(|v| v.path().to_path_buf())).collect()
    }

    /// The device peers issue fabric frames against. The control plane publishes it
    /// through nvmet; it is not a consumer volume and holds no bytes of its own.
    pub fn fabric(&self) -> &std::path::Path {
        self.fabric.path()
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

    /// Build a configuration: open the device, install `cfg`, and attach a ublk device
    /// per volume plus the fabric device.
    ///
    /// Declare-or-not: the runtime keeps a device that is re-declared, tears down one
    /// that is not, and never disturbs a live registration, so adding or removing a
    /// volume or a peer is just a different set of calls this time round. Re-placing an
    /// extent touches no device at all — the new config carries it.
    pub fn attach(&self, c: &Configurator, cfg: Config) -> std::io::Result<Dataplane> {
        let dev = std::path::PathBuf::from(&cfg.node.device);
        let limit = runtime::Limiter::new(
            cfg.node.device_max_iops,
            cfg.node.device_max_bytes_per_sec,
        );
        let disk = c.disk(&dev, None, Some(limit))?;
        let cores = c.cores();
        let share_slots = cfg.share_slots();
        // Declare everything the new config asks for *before* publishing it anywhere. A
        // failure below is a rejected config: the runtime discards the build, so the
        // allocator must still be reading the config that is actually running.
        let mut volumes = Vec::new();
        for v in &cfg.volumes {
            // A frame names a page by index within its volume, in a field narrower than
            // the config's own limit. Checked here rather than in `config` so the config
            // format stays independent of the wire format.
            let cap = if v.huge { fabric::MAX_HUGE_PAGES } else { fabric::MAX_SMALL_PAGES };
            if v.pages() > cap {
                return Err(std::io::Error::other(format!(
                    "volume {} has {} pages, more than the fabric can address",
                    v.id,
                    v.pages()
                )));
            }
            volumes.push(c.volume(v.id as u64, v.bytes(), v.huge)?);
        }
        // Sparse by construction: the fabric device is an address space, not storage.
        let fabric = c.fabric(FABRIC_KEY, fabric::DEVICE_SIZE)?;
        let mut links = Vec::new();
        for p in &cfg.node.peers {
            links.push(Link::open(c, p)?);
        }

        // Point of no return: nothing past here can fail on a reload.
        let paxos = match self.paxos.get() {
            Some(&p) => {
                p.alloc().install(cfg);
                p
            }
            None => {
                let alloc = alloc::open(&dev, disk, cfg, cores)?;
                // One metric row per worker: this is the first point where the worker
                // count is settled.
                metrics::init(cores);
                // Consensus is leaked for the same reason the allocator is: a hop closure
                // must be `'static`, and both live as long as the process anyway.
                let p = paxos::open(alloc, cache::open(alloc, cores), cores);
                let _ = self.paxos.set(p);
                let _ = self.heal.set(heal::open(p, cores));
                p
            }
        };
        paxos.install_links(links);
        // The cohort roster is derived from the topology, so it is re-derived beside the
        // allocator's own config swap.
        paxos.cache().install(paxos.alloc().config());
        let heal = *self.heal.get().expect("set beside paxos");
        Ok(Dataplane { paxos, cache: paxos.cache(), heal, volumes, fabric, share_slots })
    }
}

impl Handler for Server {
    type Config = Dataplane;

    async fn handle(&'static self, cfg: Cfg<Dataplane>, req: Request) -> Result<(), Errno> {
        if req.vol == FABRIC_KEY {
            // Boxed: this future is much larger than a volume request's, and every
            // worker preallocates one slot per tag sized for the larger of the two.
            return Box::pin(dispatch(&cfg, req)).await;
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
        cache_hits: c.hits,
        cache_misses: c.misses,
        cache_served: c.served,
        cache_admits: c.admits,
        cache_evictions: c.evictions,
        cache_stale: c.stale,
        cache_shed: c.shed,
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
        // A reload can add a volume the formatted device has no slots for. Growing needs
        // a restart, so until then the shortfall is a number to alert on rather than a
        // silent ENOSPC once the free lists run down.
        s.alloc_unbacked = layout::shortfall(&a.geometry(), cfg);
        // Device-wide rather than per core, so only one worker reports it.
        s.device_throttle_us = a.device_waited_us();
        s.config_generation = cfg.generation;
        s.config_rejected = config::rejected();
        s.topology_epoch = cfg.topology.epoch as u64;
        s.node_id = cfg.node.id as u64;
        // What the control plane sizes a rebalance against: the share it has given this
        // node, and the ceiling the device was formatted for.
        s.share_slots = d.share_slots as u64;
        s.max_share_slots = cfg.node.max_share_slots as u64;
        s.workers = a.cores() as u64;
        s.volumes = cfg.volumes.len() as u64;
        s.peers = cfg.node.peers.len() as u64;
    }
    metrics::publish(core, &s);
    // Per volume, and outside `Sample` because the rows exist only for volumes a
    // configuration names. Every named volume gets a row even when this core holds
    // nothing for it: the control plane gates an epoch advance on a zero, and a series
    // that never appeared is not one.
    let cfg = a.config();
    let census = a.census();
    let vols: Vec<(u8, u32, u64, u64)> = cfg
        .volumes
        .iter()
        .map(|v| match census.binary_search_by_key(&v.id, |c| c.0) {
            Ok(i) => (v.slot, v.id, census[i].1, census[i].2),
            Err(_) => (v.slot, v.id, 0, 0),
        })
        .collect();
    metrics::publish_volumes(core, &vols);
}

/// The consumer path. Every mutation is a guarded accept and every read is a quorum
/// read; consensus owns the guard, the ballot and the fan-out, leaving address
/// arithmetic and the two rules.
async fn serve(d: &Dataplane, req: Request) -> Result<(), Status> {
    let a = d.alloc();
    let px = d.paxos;
    let vol = req.vol as u32;
    let huge = a.config().volume(vol).map(|v| v.huge).ok_or(Status::Unmapped)?;

    // At the low watermark we slow completions instead of failing. The tag stays
    // outstanding, so blk-mq's queue depth bounds what the kernel hands us next.
    if req.op == Op::Write && a.pressure() == Pressure::Low {
        sleep(Duration::from_micros(200)).await;
    }

    if huge {
        let addr = GlobalAddr::new(vol, (req.lba / 1024) as u32);
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
        let addr = GlobalAddr::new(vol, req.lba as u32);
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
async fn huge_read(d: &Dataplane, addr: GlobalAddr, off: usize, req: Request) -> Result<(), Status> {
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
async fn dispatch(d: &Dataplane, req: Request) -> Result<(), Errno> {
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
    if let Some(addr) = routed(d, f)?
        && !d.paxos.serves(f.op, addr, f.imm)
    {
        return relay(d, f, addr, req).await;
    }

    match f.op {
        fabric::Op::Get => get(d, f, part, req).await,
        fabric::Op::Trim => trim(d, f, part, req).await,
        fabric::Op::Ping => ping(d, part, req).await,
        fabric::Op::Accept => accept(d, f, part, req).await,
        fabric::Op::GetMeta => get_meta(d, f, part, req).await,
        fabric::Op::Prepare => prepare(d, f, part, req).await,
        fabric::Op::Learn => learn(d, f, part, req).await,
        fabric::Op::Seal => seal(d, f, part, req).await,
        fabric::Op::Merkle => merkle(d, f, part, req).await,
        fabric::Op::SnapOpen => snap_open(d, f, part, req).await,
        fabric::Op::SnapNext => snap_next(d, f, part, req).await,
        fabric::Op::Term => term(d, f, part, req).await,
    }
}

/// The page a frame is addressed to, if it is one this node might have to forward.
///
/// Group ops name a group rather than a page and arrive by the sender's choice of link,
/// so they are always ours. `CACHE_ONLY` is excluded for the same reason: it asks
/// whoever holds a cached copy, which is a cohort replica and usually not a member of
/// the group at all.
fn routed(d: &Dataplane, f: Frame) -> Result<Option<GlobalAddr>, Errno> {
    use fabric::Op::*;
    match f.op {
        Get | Trim | Accept | GetMeta | Prepare | Learn | Seal
            if f.flags & fabric::CACHE_ONLY == 0 =>
        {
            Ok(Some(addr_of(d, f)?))
        }
        _ => Ok(None),
    }
}

/// Frame address to allocator address.
///
/// A frame names a volume by its fabric slot, so this is also the bounds check that
/// keeps a peer's arithmetic from reaching a page that is not there. An address past the
/// end is a bad frame, not a misrouted one: routing happens above this.
fn addr_of(d: &Dataplane, f: Frame) -> Result<GlobalAddr, Errno> {
    let v = d.alloc().config().volume_at(f.vol).ok_or(status::BAD)?;
    if v.huge != f.huge || f.offset as u64 >= v.pages() {
        return Err(status::BAD);
    }
    Ok(GlobalAddr::new(v.id, f.offset))
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
async fn get(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    let a = d.alloc();
    let addr = addr_of(d, f)?;
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
        let Part::Payload { off } = part else { return Err(status::BAD) };
        // Unlike the volume path, a hole is not zeroes here. A consumer reading an
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
    let r = d.paxos.read_for(addr, Sink::Small(&mut page)).await.map_err(wire)?;
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
        let Part::Payload { off } = part else { return Err(status::BAD) };
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
    let r = d.cache.load(addr, false, 0, buf).await.ok_or(status::MISSING)?;
    // The register the entry claims travels with it; the reader confirms it against the
    // quorum and drops the entry if it does not match. No width: we are a replica here,
    // not the owner, and have no business advertising one.
    paxos::put_register(&mut page[fabric::BLOCK..], r, 0);
    req.store(0, &page)
}

/// `TRIM`: delete a page. A guarded accept whose value is a tombstone, so a repeat says
/// nothing new.
async fn trim(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let addr = addr_of(d, f)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    req.load(0, &mut t)?;
    d.paxos.accept_trim(addr, f.imm, &t).await.map_err(wire)
}

/// `ACCEPT`: apply a page under a ballot. A 4 KiB frame gathers its guard and ballot
/// into a trailer beside the page; a 4 MiB frame is all payload and has neither, so the
/// acceptor derives both — legal only because the 4 MiB class is Immutable, whose guard
/// every replica computes from the config alone.
async fn accept(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    let addr = addr_of(d, f)?;
    if !paxos::accept_parts(f.huge, part) {
        return Err(status::BAD);
    }
    if f.huge {
        let Part::Payload { off } = part else { return Err(status::BAD) };
        // A transport splits a 4 MiB command at its MDTS, so the page can arrive as
        // consecutive pieces. One whole page still takes the path it always did.
        if off == 0 && req.buf.len() as u64 == layout::HUGE_PAGE {
            return d.paxos.accept(addr, f.imm, None, Page::Huge(req.buf)).await.map_err(wire);
        }
        return d.paxos.accept_part(addr, f.imm, off as u32, req.buf).await.map_err(wire);
    }
    let mut both = PoolBuf::alloc(2 * fabric::BLOCK).await;
    req.load(0, &mut both)?;
    let (page, trailer) = both.split_at(fabric::BLOCK);
    // Staged into our own memory so the checksum covers bytes nobody else can change,
    // as the volume path does.
    let mut p = PoolBuf::alloc(fabric::BLOCK).await;
    p.copy_from_slice(page);
    let t = trailer.to_vec();
    d.paxos.accept(addr, f.imm, Some(&t), Page::Small(&p)).await.map_err(wire)
}

/// `GETMETA`: the metadata half of a hedged read. No page moves, and the answer is a
/// register the owning core already holds.
///
/// Under [`fabric::CACHE_ONLY`] it answers for our *cached* copy instead: the register
/// half of a 4 MiB cache hit, whose class has no trailer to gather one into, so it rides
/// a second command beside the page.
async fn get_meta(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let addr = addr_of(d, f)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    if f.flags & fabric::CACHE_ONLY != 0 {
        if d.cache.shedding() {
            return Err(status::MISSING);
        }
        let r = d.cache.peek_immutable(addr, f.huge).await.ok_or(status::MISSING)?;
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
async fn prepare(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let addr = addr_of(d, f)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    d.paxos.prepare(addr, &mut t).await.map_err(wire)?;
    req.store(0, &t)
}

/// `LEARN`: a value we may be behind on, and the member holding it. Apply-if-newer, so
/// a repeat is free and the repair and migration streams commute.
async fn learn(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let addr = addr_of(d, f)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    req.load(0, &mut t)?;
    let (r, from, repair) = paxos::learn_trailer(&t);
    d.paxos.learn(addr, r, from, repair).await.map_err(wire)
}

/// `SEAL`: freeze a shard at its source group. An ordinary accept whose value happens
/// to be a shard rather than a page.
async fn seal(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let v = d.alloc().config().volume_at(f.vol).ok_or(status::BAD)?;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    req.load(0, &mut t)?;
    let (id, term) = paxos::seal_trailer(v.id, &t);
    d.paxos.seal(id, term).await.map_err(wire)
}

/// The three anti-entropy ops. Their frames name a *consensus group*, not a page, so
/// `addr_of` is deliberately off these paths and `vol`/`offset` mean what `heal.rs` says
/// they mean rather than what the frame layout says.
///
/// `MERKLE`: our digest vector for one group and class. A group we hold nothing for
/// answers all zeroes rather than an error — the correct digest of an empty set, which a
/// peer that also holds nothing has to agree with.
async fn merkle(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let group = f.offset;
    if group as usize >= d.alloc().config().topology.catalog.len() {
        return Err(status::BAD);
    }
    let v = d.alloc().digests(group, f.imm & 1 == 1).await;
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    heal::put_digests(&mut t, &v);
    req.store(0, &t)
}

/// `SNAPOPEN`: begin enumerating a group's registers, optionally filtered to one digest
/// bucket. Reply slot 0 is the cursor id, which is self-describing so `SNAPNEXT` needs
/// nothing else. `NOSPC` means this slab already holds as many cursors as it will.
async fn snap_open(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let (group, huge, bucket) = heal::snap_open_parts(&f);
    if group as usize >= d.alloc().config().topology.catalog.len() {
        return Err(status::BAD);
    }
    let id = d
        .alloc()
        .snap_open(group, huge, bucket.map_or(heal::Filter::All, heal::Filter::Bucket))
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
async fn snap_next(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let (id, seq) = heal::snap_next_parts(&f);
    let (tuples, done) = d.alloc().snap_next(id, Some(seq)).await.map_err(wire)?;
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
async fn term(d: &Dataplane, f: Frame, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let group = f.offset;
    if group as usize >= d.alloc().config().topology.catalog.len() {
        return Err(status::BAD);
    }
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    d.paxos.term(group, &mut t).await.map_err(wire)?;
    req.store(0, &t)
}

/// `PING`: liveness, and the geometry a caller needs to tell a stale answer from a
/// current one. Trailer slots: 0 node id, 1 config generation, 2 topology epoch.
async fn ping(d: &Dataplane, part: Part, req: Request) -> Result<(), Errno> {
    if part != Part::Trailer {
        return Err(status::BAD);
    }
    let mut t = PoolBuf::alloc(4096).await;
    t.fill(0);
    let c = d.alloc().config();
    fabric::put(&mut t, 0, c.node.id as u64);
    fabric::put(&mut t, 1, c.generation);
    fabric::put(&mut t, 2, c.topology.epoch as u64);
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
async fn relay(d: &Dataplane, f: Frame, addr: GlobalAddr, req: Request) -> Result<(), Errno> {
    // No budget, or a member we hold no link to: either way the originator has our
    // placement wrong, so it is sent back to its config rather than told the page is
    // missing.
    if fabric::hops(f.flags) == 0 {
        return Err(status::STALE);
    }
    let (link, crossing) = d.paxos.forward_link(f.op, addr, f.imm).ok_or(status::STALE)?;
    // A crossing is bounded by the address, not by the budget: past it the address is
    // site-local, so it restores the budget the far site's own hops will need.
    let out = if crossing { f.refreshed() } else { f.forwarded() };
    link.send(out, req.buf).await
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

    use crate::alloc::GlobalAddr;
    use crate::config::Config;
    use crate::fabric;
    use crate::heal;
    use crate::layout::{self, Class, State};
    use crate::runtime;
    use super::{self as server, SERVER};

    const IMG: &str = "racer-e2e-alloc.img";
    const DEV_BYTES: u64 = 1 << 30;

    /// Small volume 1 is LWW, small volume 2 is OCC, huge volume 3 is immutable. Eight
    /// groups so addresses spread over the workers and the cross-core paths are used.
    ///
    /// Volume 4 is homed in zone 2, which this node is not in and holds no link to, so
    /// routing a foreign address is exercised rather than resolved. Volume 5 is homed
    /// here but on its way to zone 2, so it serves locally until it is sealed and
    /// forwards afterwards.
    ///
    /// No volume is homed in another site: reaching one takes a peer holding a crossing
    /// or named gateway for it, and a peer here would give this node consensus to heal
    /// from, which is the one thing the rest of this test needs it not to have.
    ///
    /// This node is a member of every group. It has to be: consensus routes a page to
    /// the group that owns it, and with no peers declared a group we are not in has no
    /// reachable member at all. A single node is member index 0 everywhere, so its
    /// quorum is one and a local accept is a decision.
    fn config_text(dev: &Path) -> String {
        format!(
            "
            generation 1
            node id=1 zone=1 device={}
            group 1 2 3
            group 1 4 5
            group 1 6 7
            group 1 8 9
            group 1 10 11
            group 1 12 13
            group 1 14 15
            group 1 16 17
            slots round_robin
            zone id=2 entry=2,3,4
            volume 1 slot=5
              extent pages=4096 kind=lww zone=1
            volume 2 slot=1
              extent pages=256 kind=occ zone=1
            volume 3 slot=9
              extent pages=2 kind=immutable_4m zone=1
            volume 4 slot=7
              extent pages=64 kind=lww zone=2
            volume 5 slot=11
              extent pages=64 kind=lww zone=1 next_zone=2
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

    /// Bring the dataplane up and hand back the ublk path of each declared volume,
    /// ordered by volume id, plus the fabric device's. `devices()` reports volumes in
    /// config order, which is slot order, not id order.
    fn up(cfg: Config) -> (runtime::Runtime<server::Dataplane>, Vec<PathBuf>, PathBuf) {
        let rt = runtime::start(&SERVER).expect("start");
        let found = Arc::new(Mutex::new((Vec::new(), PathBuf::new())));
        let out = found.clone();
        let node = server::Node::new();
        rt.reload(move |c| {
            let d = node.attach(c, cfg)?;
            let mut devs = d.devices();
            devs.sort_by_key(|(id, _)| *id);
            *out.lock().unwrap() =
                (devs.into_iter().map(|(_, p)| p).collect(), d.fabric().to_path_buf());
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
        std::fs::OpenOptions::new().read(true).write(true).open(p).unwrap()
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
        (0..len).map(|i| seed ^ (i as u8).wrapping_mul(31)).collect()
    }

    /// A frame issued against our own fabric device, which is what a peer's link would
    /// do: the peer's namespace *is* this block device, so driving it locally exercises
    /// every line the remote path would except the transport itself.
    fn frame_read(f: &layout::Dev, fr: fabric::Frame, len: usize) -> std::io::Result<Vec<u8>> {
        let mut buf = layout::Aligned::new(len);
        layout::read_at(&f, buf.as_mut(), fr.encode() * 4096)?;
        Ok(buf.as_ref().to_vec())
    }

    fn frame_write(f: &layout::Dev, fr: fabric::Frame, data: &[u8]) -> std::io::Result<()> {
        let mut buf = layout::Aligned::new(data.len());
        buf.as_mut().copy_from_slice(data);
        layout::write_at(&f, buf.as_ref(), fr.encode() * 4096)
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
        {
            let f = std::fs::File::create(&dev).unwrap();
            f.set_len(DEV_BYTES).unwrap();
        }
        let cfg = Config::parse(&config_text(&dev)).unwrap();
        cfg.validate().unwrap();
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
            assert_eq!(read_at(&mut lww, p * 4096, 4096).unwrap(), pattern(p as u8, 4096));
        }

        // A never-written page is a hole and reads as zeroes.
        assert_eq!(read_at(&mut lww, 1000 * 4096, 4096).unwrap(), vec![0u8; 4096]);

        // Overwrite in place from the client's point of view; out of place underneath.
        write_at(&mut lww, 0, &small);
        assert_eq!(read_at(&mut lww, 0, 4096).unwrap(), small);

        // Discard is a trim, and a trimmed page is one the volume no longer has: to a
        // block client that is indistinguishable from one never written.
        let range = [63 * 4096u64, 4096u64];
        let rc = unsafe { libc::ioctl(lww.as_raw_fd(), 0x1277 /* BLKDISCARD */, range.as_ptr()) };
        assert_eq!(rc, 0, "discard: {}", std::io::Error::last_os_error());
        assert_eq!(direct_read(&paths[0], 63 * 4096, 4096).unwrap(), vec![0u8; 4096]);

        // Immutable: the first whole-page fill lands, a second one is refused.
        write_at(&mut imm, 0, &huge);
        assert_eq!(read_at(&mut imm, 0, 4 << 20).unwrap(), huge);
        imm.seek(SeekFrom::Start(0)).unwrap();
        assert!(
            imm.write_all(&huge).and_then(|_| imm.sync_data()).is_err(),
            "CORFU fill must refuse a second write"
        );

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
        // The fabric names a volume by the slot the control plane gave it, which is
        // neither its id nor its position: the config above gives volumes 1, 2, 3 the
        // slots 5, 1, 9 precisely so a frame using either instead would miss.
        let lww_slot = 5u8; // volume 1
        let imm_slot = 9u8; // volume 3

        // O_DIRECT so every frame is a real command: a frame is a request, not an
        // address, and a cached reply would answer a different question.
        let fab = layout::open_direct(&fab_path, true).unwrap();
        write_at(&mut lww, 7 * 4096, &small);

        // ---- GET, bare: the reply is the payload -------------------------------
        let get7 = fabric::Frame::new(fabric::Op::Get, false, lww_slot, 7);
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), small);

        // ---- GET, gather: page then trailer, one command ------------------------
        // The trailer is the register the page was chosen at: value and proof in one
        // command.
        let both = frame_read(&fab, get7, 8192).unwrap();
        assert_eq!(&both[..4096], &small[..]);
        let v7 = fabric::get(&both[4096..], 0);
        assert_ne!(v7, 0, "a page that exists was written at some version");
        assert_ne!(fabric::get(&both[4096..], 1), 0, "and at the ballot that chose it");

        // ---- A page this member does not have is not zeroes, it is MISSING ------
        let gone = fabric::Frame::new(fabric::Op::Get, false, lww_slot, 999);
        let e = frame_read(&fab, gone, 4096).unwrap_err();
        assert_eq!(errno(e), libc::ENODATA, "a hole is a hole, not a page of zeroes");
        // The same page over the volume device still reads as zeroes: that is what the
        // block layer requires of a consumer.
        assert_eq!(read_at(&mut lww, 999 * 4096, 4096).unwrap(), vec![0u8; 4096]);

        // ---- Addressing: past the end of the volume, and the wrong page class ----
        let past = fabric::Frame::new(fabric::Op::Get, false, lww_slot, 4096);
        assert_eq!(errno(frame_read(&fab, past, 4096).unwrap_err()), libc::EOPNOTSUPP);
        let miscl = fabric::Frame::new(fabric::Op::Get, true, lww_slot, 0);
        assert_eq!(errno(frame_read(&fab, miscl, 4096).unwrap_err()), libc::EOPNOTSUPP);
        let novol = fabric::Frame::new(fabric::Op::Get, false, 60, 0);
        assert_eq!(errno(frame_read(&fab, novol, 4096).unwrap_err()), libc::EOPNOTSUPP);

        // ---- GET, 4 MiB: whole, and in the pieces an MDTS split would produce ----
        let geth = fabric::Frame::new(fabric::Op::Get, true, imm_slot, 0);
        assert_eq!(frame_read(&fab, geth, 4 << 20).unwrap(), huge);
        for (block, len) in [(0usize, 256 << 10), (64, 256 << 10), (1023, 4096)] {
            let mut b = layout::Aligned::new(len);
            let off = (geth.encode() + block as u64) * 4096;
            layout::read_at(&fab, b.as_mut(), off).unwrap();
            assert_eq!(b.as_ref(), &huge[block * 4096..block * 4096 + len], "block {block}");
        }

        // ---- PING: liveness and the geometry that dates an answer ---------------
        let ping = fabric::Frame::new(fabric::Op::Ping, false, 0, 0);
        let t = frame_read(&fab, ping, 4096).unwrap();
        assert_eq!(fabric::get(&t, 0), 1, "node id");
        assert_eq!(fabric::get(&t, 1), 1, "config generation");
        assert_eq!(fabric::get(&t, 2), 0, "topology epoch");
        // Control ops are exactly one block.
        assert!(frame_read(&fab, ping, 8192).is_err());

        // A small ACCEPT must gather page and trailer into one command; a page on its
        // own is a bad frame.
        let acc7 = fabric::Frame::new(fabric::Op::Accept, false, lww_slot, 7);
        assert_eq!(errno(frame_write(&fab, acc7, &small).unwrap_err()), libc::EOPNOTSUPP);

        // ---- GETMETA: the register, and not one byte of the page -----------------
        let meta7 = fabric::Frame::new(fabric::Op::GetMeta, false, lww_slot, 7);
        let t = frame_read(&fab, meta7, 4096).unwrap();
        assert_eq!(fabric::get(&t, 0), v7, "the metadata read and the gathered trailer agree");
        assert_ne!(fabric::get(&t, 1), 0, "a chosen value carries the ballot that chose it");

        // ---- ACCEPT: one round trip, and the guard is the collision detector ------
        // The page and the trailer travel together, so a whole proposal is one command:
        // `imm` zero says the sender is not a member and we are the proposer.
        let next = pattern(0x11, 4096);
        let mut cmd = vec![0u8; 8192];
        cmd[..4096].copy_from_slice(&next);
        fabric::put(&mut cmd[4096..], 0, v7);
        frame_write(&fab, acc7, &cmd).expect("an accept at the version we hold");
        // Read back over the fabric, not the volume: the volume fd is buffered and
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
        let prep7 = fabric::Frame::new(fabric::Op::Prepare, false, lww_slot, 7);
        let t = frame_read(&fab, prep7, 4096).unwrap();
        assert_eq!(fabric::get(&t, 0), v7 + 1, "prepare reports, it does not write");
        let term1 = fabric::get(&t, 2);
        assert!(term1 >= 1, "a prepare that raised nothing granted nothing");
        let t = frame_read(&fab, prep7, 4096).unwrap();
        let term2 = fabric::get(&t, 2);
        assert!(term2 > term1, "every prepare raises the promise");

        // ---- LEARN: apply-if-newer, so one we are ahead of costs nothing ---------
        let learn7 = fabric::Frame::new(fabric::Op::Learn, false, lww_slot, 7);
        let mut lt = vec![0u8; 4096];
        fabric::put(&mut lt, 0, 1); // version 1: older than what we hold
        frame_write(&fab, learn7, &lt).expect("a learn we are ahead of is a no-op");
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), next);

        // ---- MERKLE and the cursor: healing --------------------------------------
        // Both carry registers and never page bytes, so a digest and a chunk of a
        // snapshot each ride the trailer of one control block.
        let addr7 = GlobalAddr::new(1, 7);
        let group = cfg.group(addr7.0);
        let bucket = heal::bucket_of(addr7.0);
        let groups = cfg.topology.catalog.len() as u32;

        // The page is filed under the group and the bucket its address hashes to.
        let tree = heal::get_digests(&frame_read(&fab, heal::merkle_frame(group, false), 4096).unwrap());
        assert_ne!(tree[bucket as usize], 0, "the digest is under the address's own bucket");
        // The classes are separate trees, so the small page is not in the huge one.
        // Zeroes are the digest of the empty set, not a refusal.
        let huge_tree = heal::get_digests(&frame_read(&fab, heal::merkle_frame(group, true), 4096).unwrap());
        assert_eq!(huge_tree[bucket as usize], 0, "a class is not the other class");
        // A group outside the catalog is a frame built against a topology we do not
        // share, which is not the same as a group that happens to be empty.
        let no_group = heal::merkle_frame(groups, false);
        assert_eq!(errno(frame_read(&fab, no_group, 4096).unwrap_err()), libc::EOPNOTSUPP);

        // The bucket-filtered cursor names the page and the register GETMETA reported,
        // and says it is finished in the same reply.
        let open = heal::snap_open_frame(group, false, Some(bucket));
        let id = fabric::get(&frame_read(&fab, open, 4096).unwrap(), 0) as u32;
        let mut seen = std::collections::BTreeMap::new();
        let done = heal::get_tuples(&frame_read(&fab, heal::snap_next_frame(id, 0), 4096).unwrap(), &mut seen);
        assert!(done, "one bucket of one group fits in one chunk");
        assert!(
            seen.keys().all(|a| heal::bucket_of(*a) == bucket),
            "the filter is a filter: {seen:?}"
        );
        assert_eq!(seen[&addr7.0].version, v7 + 1, "the cursor and the register agree");
        // The last chunk closes the cursor: there is no close on the wire.
        assert_eq!(
            errno(frame_read(&fab, heal::snap_next_frame(id, 0), 4096).unwrap_err()),
            libc::EOPNOTSUPP,
            "a cursor that finished is gone, not stale"
        );
        // The filter narrows the walk without changing what the walk is over.
        let all = heal::snap_open_frame(group, false, None);
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
        assert_eq!(errno(frame_write(&fab, get7, &small).unwrap_err()), libc::EOPNOTSUPP);

        // ---- Forwarding: addressed elsewhere, and no link is EREMOTEIO -----------
        // `imm` names the addressee — `k + 1` is member index `k`, and `0` is "you
        // resolve it". This node is index 0 of every group here, so anything else is
        // a frame to pass on, and the config has no peers, so every forward is
        // EREMOTEIO.
        let mut fwd = fabric::Frame::new(fabric::Op::Get, false, lww_slot, 7);
        fwd.imm = 2;
        fwd.flags = 1;
        assert_eq!(
            errno(frame_read(&fab, fwd, 4096).unwrap_err()),
            libc::EREMOTEIO,
            "no link to the named member is, to the originator, a placement it has wrong"
        );
        // A control read forwards the same way, and so does a write.
        let mut meta_fwd = fabric::Frame::new(fabric::Op::GetMeta, false, lww_slot, 7);
        meta_fwd.imm = 2;
        meta_fwd.flags = 1;
        assert_eq!(errno(frame_read(&fab, meta_fwd, 4096).unwrap_err()), libc::EREMOTEIO);
        let mut trim_fwd = fabric::Frame::new(fabric::Op::Trim, false, lww_slot, 7);
        trim_fwd.imm = 2;
        trim_fwd.flags = 1;
        assert_eq!(errno(frame_write(&fab, trim_fwd, &[0u8; 4096]).unwrap_err()), libc::EREMOTEIO);
        // A frame for someone else with no budget left is refused rather than passed
        // on: the budget terminates the recursion.
        let mut spent = fabric::Frame::new(fabric::Op::Get, false, lww_slot, 7);
        spent.imm = 2;
        assert_eq!(fabric::hops(spent.flags), 0);
        assert_eq!(errno(frame_read(&fab, spent, 4096).unwrap_err()), libc::EREMOTEIO);
        // Addressed to us — either by name or by `imm == 0` — and served normally.
        let mut mine = fabric::Frame::new(fabric::Op::Get, false, lww_slot, 7);
        mine.imm = 1;
        assert_eq!(frame_read(&fab, mine, 4096).unwrap(), next);
        let resolve = fabric::Frame::new(fabric::Op::Get, false, lww_slot, 7);
        assert_eq!(frame_read(&fab, resolve, 4096).unwrap(), next);

        // ---- Cross-zone: homed elsewhere, so routed and never resolved here ------
        // Volume 4 lives in zone 2. Our slot table describes our own zone only, so
        // resolving one of its addresses against it would name a group in the wrong
        // zone; with no link to zone 2's entry nodes there is nowhere to send it.
        let away = fabric::Frame::new(fabric::Op::Get, false, 7, 0);
        assert_eq!(
            errno(frame_read(&fab, away, 4096).unwrap_err()),
            libc::EREMOTEIO,
            "a foreign address is a placement the originator has wrong, not a hole"
        );
        {
            let mut afar = open_dev(&paths[3]);
            assert!(read_at(&mut afar, 0, 4096).is_err(), "a foreign page is not answered locally");
        }

        // ---- TRIM: last, because it destroys the page it names -------------------
        let trim = fabric::Frame::new(fabric::Op::Trim, true, imm_slot, 0);
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
        let seal = fabric::Frame::new(fabric::Op::Seal, false, lww_slot, 7);
        let mut st = vec![0u8; 4096];
        fabric::put(&mut st, 1, term2 + 1);
        fabric::put(&mut st, 2, 0);
        frame_write(&fab, seal, &st).expect("seal");
        assert_eq!(
            errno(frame_write(&fab, acc7, &cmd).unwrap_err()),
            libc::EREMOTEIO,
            "a sealed shard accepts nothing further"
        );
        assert_eq!(frame_read(&fab, get7, 4096).unwrap(), next, "a seal does not stop reads");

        // A shard on its way somewhere forwards instead of refusing, so a client holding
        // a config from before the flip still gets an answer. Zone 2's entry node is not
        // a peer of ours, so the forward fails at the link rather than at the gate —
        // that is the distinction asserted: `EIO` is "routed and unreachable",
        // `EREMOTEIO` is "refused".
        //
        // The seal is redundant — the sweep seals an extent with `next_zone` set of its
        // own accord — and is here only so the assertion does not race the tick.
        let away_slot = 11u8; // volume 5, zone 1 -> zone 2
        let acc_away = fabric::Frame::new(fabric::Op::Accept, false, away_slot, 3);
        let seal_away = fabric::Frame::new(fabric::Op::Seal, false, away_slot, 3);
        frame_write(&fab, seal_away, &st).expect("seal");
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
        let small_addr = GlobalAddr::new(1, 1);
        let huge_addr = GlobalAddr::new(3, 1);
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
        assert_eq!(direct_read(&paths[0], 2 * 4096, 4096).unwrap(), pattern(2, 4096));

        // A ublk device cannot be deleted while a client still holds it open, so the
        // client always lets go first.
        drop((lww, imm, fab));
        rt.shutdown().unwrap();
        let _ = std::fs::remove_file(&dev);
    }
}
