//! Serve the client and fabric facing ublk devices.

use std::rc::Rc;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use crate::alloc::{self, Allocator, GlobalAddr, Pressure, Status};
use crate::cache::{self, Cache};
use crate::config::{self, Config, Device};
use crate::fabric::{self, Class, Cmd, Footer, GroupIx, Link, PageRef, Source, To, Want, status};
use crate::layout;
use crate::metrics;
use crate::paxos::heal::{self, Heal};
use crate::paxos::{self, Page, Paxos, Register, Sink};
use crate::runtime::{
    self, Errno, Export, Handler, Limiter, Op, PoolBuf, Request, ResourceBuild, sleep,
};

/// Abstract representation of a single node in the Racer fabric.
#[derive(Default)]
pub struct Node {
    paxos: std::sync::OnceLock<&'static Paxos>,
}

impl Node {
    pub fn new() -> Node {
        Node::default()
    }

    pub fn build_generation(
        &self,
        c: &ResourceBuild,
        previous: Option<&NodeGeneration>,
        cfg: Config,
    ) -> std::io::Result<Option<NodeGeneration>> {
        if let Some(previous) = previous {
            if cfg.generation <= previous.env.config().generation {
                return Ok(None);
            }
            cfg.validate()?;
            cfg.validate_against(previous.env.config())?;
        } else {
            cfg.validate()?;
        }
        // Store
        let store = cfg.node.store.clone();
        let limit = Limiter::new(cfg.node.max_iops(), cfg.node.max_bytes_per_sec());
        let disk = c.disk(&store, None, Some(limit))?;

        // Client-facing devices
        let mut devices = Vec::new();
        for dev in cfg.devices() {
            let e = c.device(dev.id as u64, dev.id, dev.bytes())?;
            devices.push((dev.id, e));
        }

        // Peer-facing (fabric) devices
        let mut universes = Vec::new();
        for u in cfg.universes() {
            let e = c.fabric(fabric::key(u.id), u.fabric_device_id, fabric::DEVICE_SIZE)?;
            universes.push((u.id, e));
        }
        let mut links = Vec::new();
        for (universe, p) in cfg.peers() {
            links.push(Link::open(c, universe, p)?);
        }

        let cores = c.cores();
        let mut rows = Vec::new();
        let paxos = match self.paxos.get() {
            Some(&p) => p,
            None => {
                let (alloc, shards) = alloc::open(&store, disk, &cfg, cores)?;
                metrics::init(cores);

                let (cache, slots) = cache::open(alloc, &cfg, cores);

                let (p, terms) = paxos::open(alloc, cache, cores);
                let _ = self.paxos.set(p);
                p.attach_heal(heal::open(p));
                rows = CoreState::all(shards, slots, terms)
                    .into_iter()
                    .map(|row| Mutex::new(Some(row)))
                    .collect();
                p
            }
        };
        Ok(Some(NodeGeneration {
            env: config::Compiled::with_links(cfg, links),
            paxos,
            universes,
            devices,
            rows: rows.into_boxed_slice(),
        }))
    }
}

/// One configuration generation of a running [`Node`].
pub struct NodeGeneration {
    env: config::Compiled,
    paxos: &'static Paxos,
    universes: Vec<(u32, Export)>,
    devices: Vec<(u32, Export)>,
    rows: Box<[Mutex<Option<CoreState>>]>,
}

impl NodeGeneration {
    pub(crate) fn alloc(&self) -> &'static Allocator {
        self.paxos.alloc()
    }

    pub(crate) fn cache(&self) -> &'static Cache {
        self.paxos.cache()
    }

    pub(crate) fn heal(&self) -> &'static Heal {
        self.paxos.heal()
    }

    fn config(&self) -> &Config {
        self.env.config()
    }

    pub fn devices(&self) -> Vec<(u32, std::path::PathBuf)> {
        self.devices
            .iter()
            .map(|(id, v)| (*id, v.path().to_path_buf()))
            .collect()
    }

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
}

/// The state one worker thread owns outright.
pub struct CoreState {
    pub(crate) alloc: alloc::Row,
    pub(crate) cache: cache::Row,
    pub(crate) paxos: paxos::Row,
    pub(crate) heal: heal::Core,
}

impl CoreState {
    /// One row per worker, from each of the three subsystems that keep any.
    pub(crate) fn all(
        alloc: Vec<alloc::Row>,
        cache: Vec<cache::Row>,
        paxos: Vec<paxos::Row>,
    ) -> Vec<CoreState> {
        assert_eq!(alloc.len(), cache.len(), "one row per worker from each");
        assert_eq!(alloc.len(), paxos.len(), "one row per worker from each");
        alloc
            .into_iter()
            .zip(cache)
            .zip(paxos)
            .map(|((alloc, cache), paxos)| CoreState {
                alloc,
                cache,
                paxos,
                heal: heal::Core::default(),
            })
            .collect()
    }
}

pub struct Server;

pub struct Worker {
    pub(crate) config: Arc<NodeGeneration>,
    pub(crate) core: Rc<CoreState>,
}

impl std::ops::Deref for Worker {
    type Target = CoreState;

    fn deref(&self) -> &CoreState {
        &self.core
    }
}

impl Worker {
    pub(crate) fn node(&self) -> &NodeGeneration {
        &self.config
    }

    pub(crate) fn compiled(&self) -> &config::Compiled {
        &self.config.env
    }

    pub(crate) fn config(&self) -> &Config {
        self.compiled().config()
    }

    pub(crate) fn core(&self) -> &CoreState {
        &self.core
    }
}

impl Handler for Server {
    type Config = NodeGeneration;
    type Worker = Worker;

    fn build_worker(
        core: runtime::CoreId,
        config: Arc<NodeGeneration>,
        previous: Option<&Worker>,
    ) -> Worker {
        let state = match previous {
            Some(previous) => previous.core.clone(),
            None => Rc::new(
                config.rows[core.index()]
                    .lock()
                    .unwrap()
                    .take()
                    .expect("one initial state row per worker"),
            ),
        };
        Worker {
            config,
            core: state,
        }
    }

    // This is the main dataplane entrypoint!
    async fn handle(worker: Rc<Worker>, req: Request) -> Result<(), Errno> {
        if let Some(universe) = fabric::universe_of(req.dev) {
            // request is from a fabric peer
            return Box::pin(serve_peer(worker, universe, req)).await;
        }
        serve(worker, req).await.map_err(Status::errno)
    }

    fn tick(worker: Rc<Worker>, now: Instant) {
        let cfg = &worker.config;
        cfg.alloc().tick(&worker, now);
        cfg.cache().tick(worker.clone(), now);
        cfg.heal().tick(worker.clone(), now);
        sample(&worker);
    }
}

async fn serve(worker: Rc<Worker>, req: Request) -> Result<(), Status> {
    let d = worker.node();
    let a = d.alloc();
    let px = d.paxos;
    let cfg = d.config();
    let dev = cfg.device(req.dev as u32).ok_or(Status::Unmapped)?;
    let mut check = Segments::new(dev, req.lba, req.buf.len());
    while let Some(s) = check.next() {
        s?;
    }

    // Slow down the worker's ring when the allocator is under pressure.
    if req.op == Op::Write && a.pressure(&worker) == Pressure::Low {
        sleep(Duration::from_micros(200)).await;
    }

    // Slice the request into one segment per block, process them. A request spanning
    // several blocks is not atomic: each block is its own register, so an earlier one may
    // commit before a later one conflicts. The block layer is told `chunk_sectors` is one
    // block, so in practice a client request is one block anyway.
    let mut segs = Segments::new(dev, req.lba, req.buf.len());
    while let Some(s) = segs.next() {
        let s = s?;
        let addr = GlobalAddr(s.addr);
        match req.op {
            Op::Discard => px.trim(worker.clone(), addr).await?,
            Op::Read => {
                let mut page = PoolBuf::alloc(4096).await;
                match px.read(worker.clone(), addr, Sink::Small(&mut page)).await {
                    Ok(_) => {}
                    Err(Status::Hole) => page.fill(0),
                    Err(e) => return Err(e),
                }
                req.write(s.at, &page).await.map_err(|_| Status::Io)?;
            }
            Op::Write => {
                let mut page = PoolBuf::alloc(4096).await;
                req.read(s.at, &mut page).await.map_err(|_| Status::Io)?;
                px.write(worker.clone(), addr, Page::Small(&page))
                    .await
                    .map(drop)?;
            }
        }
    }
    Ok(())
}

async fn serve_peer(worker: Rc<Worker>, universe: u32, req: Request) -> Result<(), Errno> {
    let d = worker.node();
    let cmd = Cmd::decode(req.lba, req.buf.len(), req.op == Op::Read)?;

    // Forward commands that we can't handle
    if let Some((page, via)) = cmd.routing() {
        let addr = addr_of(d, universe, page)?;
        if !d.paxos.serves(&worker, cmd.op(), addr, via.to) {
            return d
                .paxos
                .forward_link(&worker, cmd.op(), addr, via.to)
                .ok_or(status::STALE)?
                .relay(cmd, req.buf)
                .await;
        }
    }

    match cmd {
        Cmd::Get { page, from, want } => peer_get(worker, universe, page, from, want, req).await,
        Cmd::GetMeta { page, from } => peer_get_meta(worker, universe, page, from, req).await,
        Cmd::Accept { page, via } => accept(worker, universe, page, via.to, req).await,

        Cmd::Prepare { page, .. } => {
            let addr = addr_of(d, universe, page)?;
            let p = d.paxos.prepare(worker, addr).await.map_err(wire)?;
            reply(&req, 0, &p).await
        }

        Cmd::Trim { page, via } => {
            let addr = addr_of(d, universe, page)?;
            let t: fabric::TrimReq = footer(&req).await?;
            d.paxos
                .accept_trim(worker, addr, via.to, t)
                .await
                .map_err(wire)
        }

        Cmd::Learn { page, .. } => {
            let addr = addr_of(d, universe, page)?;
            let l: fabric::LearnReq = footer(&req).await?;
            d.paxos
                .learn(worker, addr, l.reg.into(), l.from.index(), l.repair)
                .await
                .map_err(wire)
        }

        Cmd::Seal { .. } => {
            let s: fabric::SealReq = footer(&req).await?;
            let cfg = d.config();
            match cfg.extent_by_id(s.extent) {
                Some((u, _)) if u.id == universe => {
                    d.paxos.seal(worker, s.extent, s.term).await.map_err(wire)
                }
                _ => Err(status::BAD),
            }
        }

        Cmd::Warm { page } => {
            let addr = addr_of(d, universe, page)?;
            let w: fabric::WarmReq = footer(&req).await?;
            d.paxos.warm(worker, addr, w.version, w.stage).await;
            Ok(())
        }

        Cmd::Merkle { group, class } => {
            let group = group_of(d, universe, group)?;
            let v = d
                .alloc()
                .digests(&worker, group, class == Class::Immutable)
                .await;
            reply(&req, 0, &fabric::MerkleReply(v)).await
        }

        Cmd::SnapOpen {
            group,
            class,
            bucket,
        } => snap_open(worker, universe, group, class, bucket, req).await,
        Cmd::SnapNext { cursor, seq } => snap_next(worker, universe, cursor, seq, req).await,

        Cmd::Term { group } => {
            let group = group_of(d, universe, group)?;
            let t = d.paxos.term(group).await.map_err(wire)?;
            reply(&req, 0, &t).await
        }

        Cmd::Ping => {
            let c = d.config();
            let p = fabric::PingReply {
                node: c.node.id,
                generation: c.generation,
                epoch: c.universe(universe).map_or(0, |u| u.epoch),
            };
            reply(&req, 0, &p).await
        }
    }
}

/// Cuts a request into [`Seg`]s, one per 4 KiB block.
struct Segments<'a> {
    dev: &'a Device,
    lba: u64,
    at: usize,
    len: usize,
}

impl<'a> Segments<'a> {
    fn new(dev: &'a Device, lba: u64, len: usize) -> Segments<'a> {
        Segments {
            dev,
            lba,
            at: 0,
            len,
        }
    }

    #[allow(clippy::should_implement_trait)]
    fn next(&mut self) -> Option<Result<Seg, Status>> {
        if self.at >= self.len {
            return None;
        }
        if !(self.len - self.at).is_multiple_of(4096) {
            return Some(Err(Status::Unmapped));
        }
        let Some(addr) = self.dev.map(self.lba) else {
            return Some(Err(Status::Unmapped));
        };
        let seg = Seg { addr, at: self.at };
        self.lba += 1;
        self.at += 4096;
        Some(Ok(seg))
    }
}

/// One block worth of a request.
struct Seg {
    addr: u64,
    at: usize,
}

async fn peer_get(
    worker: Rc<Worker>,
    universe: u32,
    page: PageRef,
    from: Source,
    want: Want,
    req: Request,
) -> Result<(), Errno> {
    let d = worker.node();
    let a = d.alloc();
    let addr = addr_of(d, universe, page)?;
    let via = match from {
        Source::Cache => return cache_get(worker, want, addr, req).await,
        Source::Group(via) => via,
    };

    // Perform the operation on the group we locally determine to be its owner
    if via.to == To::Owner {
        return get_confirmed(worker, want, addr, req).await;
    }

    let mut p = PoolBuf::alloc(fabric::BLOCK).await;
    let reg = a.read_block(&worker, addr, &mut p).await.map_err(wire)?;
    req.write(0, &p).await?;

    // Add register metadata inline if requested
    if want == Want::Gather {
        let meta = d.paxos.gathered(worker, addr, reg).await.map_err(wire)?;
        return reply(&req, fabric::BLOCK, &meta).await;
    }
    Ok(())
}

async fn cache_get(
    worker: Rc<Worker>,
    want: Want,
    addr: GlobalAddr,
    req: Request,
) -> Result<(), Errno> {
    let d = worker.node();
    if d.cache().shedding(&worker) {
        return Err(status::MISSING);
    }

    // A cached read must gather: the register the copy claims rides in the trailer beside
    // the block, and there is nowhere else to put it.
    if want != Want::Gather {
        return Err(status::BAD);
    }

    let mut page = PoolBuf::alloc(2 * fabric::BLOCK).await;
    let buf = page.buf().slice(0, fabric::BLOCK);
    let r = d
        .cache()
        .load(&worker, addr, buf)
        .await
        .ok_or(status::MISSING)?;

    let meta = fabric::MetaReply {
        reg: r.into(),
        ..Default::default()
    };
    meta.encode(&mut page[fabric::BLOCK..])?;
    req.write(0, &page).await
}

async fn get_confirmed(
    worker: Rc<Worker>,
    want: Want,
    addr: GlobalAddr,
    req: Request,
) -> Result<(), Errno> {
    let d = worker.node();
    let mut page = PoolBuf::alloc(fabric::BLOCK).await;

    let r = match d.paxos.read_for(worker, addr, Sink::Small(&mut page)).await {
        Ok(r) => r,
        Err(Status::Hole) => {
            page.fill(0);
            Register::default()
        }
        Err(e) => return Err(wire(e)),
    };
    req.write(0, &page).await?;

    if want == Want::Gather {
        let meta = fabric::MetaReply {
            reg: r.into(),
            ..Default::default()
        };
        return reply(&req, fabric::BLOCK, &meta).await;
    }
    Ok(())
}

async fn peer_get_meta(
    worker: Rc<Worker>,
    universe: u32,
    page: PageRef,
    from: Source,
    req: Request,
) -> Result<(), Errno> {
    let d = worker.node();
    let addr = addr_of(d, universe, page)?;

    if from != Source::Cache {
        let meta = d.paxos.get_meta(worker, addr).await.map_err(wire)?;
        return reply(&req, 0, &meta).await;
    }

    if d.cache().shedding(&worker) {
        return Err(status::MISSING);
    }
    let r = d
        .cache()
        .peek_immutable(&worker, addr)
        .await
        .ok_or(status::MISSING)?;

    let meta = fabric::MetaReply {
        reg: r.into(),
        ..Default::default()
    };
    reply(&req, 0, &meta).await
}

async fn accept(
    worker: Rc<Worker>,
    universe: u32,
    page: PageRef,
    to: To,
    req: Request,
) -> Result<(), Errno> {
    let d = worker.node();
    let addr = addr_of(d, universe, page)?;
    let mut both = PoolBuf::alloc(2 * fabric::BLOCK).await;
    req.read(0, &mut both).await?;
    let t = fabric::AcceptReq::decode(&both[fabric::BLOCK..])?;
    both.truncate(fabric::BLOCK);
    d.paxos
        .accept(worker, addr, to, t, Page::Small(&both))
        .await
        .map_err(wire)
}

async fn snap_open(
    worker: Rc<Worker>,
    universe: u32,
    group: GroupIx,
    class: Class,
    bucket: Option<fabric::Bucket>,
    req: Request,
) -> Result<(), Errno> {
    let d = worker.node();
    let group = group_of(d, universe, group)?;
    let filter = bucket.map_or(heal::Filter::All, |b| heal::Filter::Bucket(b.get()));
    let id = d
        .alloc()
        .snap_open(worker, group, class == Class::Immutable, filter)
        .await
        .map_err(wire)?
        .into_wire();
    let r = fabric::SnapOpenReply {
        cursor: fabric::Cursor::new(id),
    };
    reply(&req, 0, &r).await
}

async fn snap_next(
    worker: Rc<Worker>,
    universe: u32,
    cursor: fabric::Cursor,
    seq: fabric::Seq,
    req: Request,
) -> Result<(), Errno> {
    let d = worker.node();
    let id = cursor.get();
    let (tuples, done) = d
        .alloc()
        .snap_next(&worker, id, Some(seq.get()), Some(universe))
        .await
        .map_err(wire)?;

    let tuples = tuples
        .iter()
        .map(|&(addr, r)| fabric::SnapTuple {
            addr,
            reg: r.into(),
        })
        .collect();

    let r = fabric::SnapNextReply::new(done, tuples).ok_or(status::BAD)?;
    if done {
        d.alloc().snap_release(&worker, id).await;
    }
    reply(&req, 0, &r).await
}

fn addr_of(d: &NodeGeneration, universe: u32, page: PageRef) -> Result<GlobalAddr, Errno> {
    let addr = config::addr_of(universe, page.lba());
    d.config().extent_at(addr).ok_or(status::BAD)?;
    Ok(GlobalAddr(addr))
}

fn group_of(d: &NodeGeneration, universe: u32, group: GroupIx) -> Result<config::GroupId, Errno> {
    let u = d.config().universe(universe).ok_or(status::BAD)?;
    if group.get() as usize >= u.catalog.len() {
        return Err(status::BAD);
    }
    Ok(config::GroupId::new(universe, group.get()))
}

fn wire(s: Status) -> Errno {
    match s {
        Status::Hole | Status::Missing => status::MISSING,
        Status::Conflict { .. } => status::STALE,
        Status::Unmapped => status::BAD,
        Status::NoSpace => status::NOSPC,
        Status::Io => Errno::EIO,
    }
}

async fn reply(req: &Request, off: usize, f: &impl Footer) -> Result<(), Errno> {
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    f.encode(&mut t)?;
    req.write(off, &t).await
}

async fn footer<F: Footer>(req: &Request) -> Result<F, Errno> {
    let mut t = PoolBuf::alloc(fabric::BLOCK).await;
    req.read(0, &mut t).await?;
    F::decode(&t)
}

fn sample(worker: &Worker) {
    let d = worker.node();
    let core = runtime::core().index();
    let a = d.alloc();
    let [mutable, immutable] = a.capacity(worker);
    let (replaying, shedding) = d.heal().outstanding(worker);
    let mut node = metrics::NodeStats {
        heal_replaying: replaying,
        heal_shedding: shedding,
        slots: [mutable.1, immutable.1],
        free: [mutable.0, immutable.0],
        ..Default::default()
    };
    match a.pressure(worker) {
        Pressure::Low => node.pressure[0] = 1,
        Pressure::Critical => node.pressure[1] = 1,
        Pressure::Normal => {}
    }

    if core == 0 {
        let cfg = d.config();
        let (tail, unused) = d.cache().tail_bytes(worker);
        node = metrics::NodeStats {
            cache_tail_bytes: tail,
            cache_unused_bytes: unused,
            quarantined: a.quarantined as u64,
            unbacked: layout::shortfall(&a.geometry(), cfg),
            store_throttle_us: a.store_waited_us(),
            config_generation: cfg.generation,
            config_rejected: config::rejected(),
            broadcast_stalls: runtime::broadcast_stalls(),
            broadcast_wait_us: runtime::broadcast_wait_us(),
            topology_epoch: cfg.universes().iter().map(|u| u.epoch).max().unwrap_or(0) as u64,
            node_id: cfg.node.id as u64,
            workers: a.cores() as u64,
            universes: cfg.universes().len() as u64,
            devices: cfg.devices().len() as u64,
            extents: cfg.extent_count() as u64,
            peers: cfg.peer_count() as u64,
            ..node
        };
    }

    metrics::publish(
        core,
        &metrics::Sample {
            paxos: d.paxos.local_stats(worker),
            heal: d.heal().local_stats(worker),
            cache: d.cache().local_stats(worker),
            alloc: a.local_stats(worker),
            node,
        },
    );

    let cfg = d.config();
    let census = a.census(worker);
    let rows: Vec<(u32, u32, u64, u64)> = cfg
        .extents()
        .map(|(u, e)| match census.binary_search_by_key(&e.id, |c| c.0) {
            Ok(i) => (u.id, e.id, census[i].1, census[i].2),
            Err(_) => (u.id, e.id, 0, 0),
        })
        .collect();
    metrics::publish_extents(core, &rows);
}

#[cfg(test)]
mod tests {
    use super::*;

    /// One device: two 4 MiB pages (2048 blocks), then three 4 KiB pages.
    const FIXTURE: &str = "\
generation 1
node id=1 zone=1 cohort=0 store=/var/lib/racer/store.img size=68719476736

universe 1 epoch=1 fabric_device_id=9
  peer id=2 device=/dev/nvme1n1
  peer id=3 device=/dev/nvme2n1
  group 1 2 3
  extent id=10 base=1024  pages=2 kind=immutable_4m zone=1
  extent id=11 base=16384 pages=3 kind=occ          zone=1

device 1 extents=10,11
";

    /// One segment of a request: `(addr, huge, off, at, len)`.
    type Cut = (u64, bool, usize, usize, usize);

    fn cut(dev: &Device, lba: u64, len: usize) -> Result<Vec<Cut>, Status> {
        let mut segs = Segments::new(dev, lba, len);
        let mut out = Vec::new();
        while let Some(s) = segs.next() {
            let s = s?;
            out.push((s.addr, s.huge, s.off, s.at, s.len));
        }
        Ok(out)
    }

    fn seg(huge: bool, off: usize, len: usize) -> Seg {
        Seg {
            addr: 0,
            huge,
            off,
            at: 0,
            len,
        }
    }

    #[test]
    fn cuts_at_page_and_span_boundaries() {
        let c = Config::parse(FIXTURE).unwrap();
        let d = c.device(1).unwrap();
        let huge = |lba| config::addr_of(1, lba);

        // Three blocks from the end of the first 4 MiB page: cut where the page ends.
        assert_eq!(
            cut(d, 1023, 3 * 4096),
            Ok(vec![
                (huge(1024), true, 1023 * 4096, 0, 4096),
                (huge(2048), true, 0, 4096, 2 * 4096),
            ])
        );

        // The last block of the device's huge extent, then its first 4 KiB one.
        assert_eq!(
            cut(d, 2047, 2 * 4096),
            Ok(vec![
                (huge(2048), true, 1023 * 4096, 0, 4096),
                (huge(16384), false, 0, 4096, 4096),
            ])
        );
    }

    #[test]
    fn refuses_unaligned_and_out_of_range_requests() {
        let c = Config::parse(FIXTURE).unwrap();
        let d = c.device(1).unwrap();

        assert_eq!(cut(d, 0, 4096 + 512), Err(Status::Unmapped));
        assert_eq!(
            cut(d, d.blocks(), 4096),
            Err(Status::Unmapped),
            "past the end"
        );
        assert_eq!(
            cut(d, d.blocks() - 1, 2 * 4096),
            Err(Status::Unmapped),
            "running off the end mid-request"
        );
        assert_eq!(cut(d, 0, 0), Ok(vec![]));
    }

    #[test]
    fn writes_take_whole_huge_pages_only() {
        let whole = layout::HUGE_PAGE as usize;

        for op in [Op::Write, Op::Discard] {
            assert_eq!(seg(true, 4096, 4096).serviceable(op), Err(Status::Unmapped));
            assert_eq!(seg(true, 0, 4096).serviceable(op), Err(Status::Unmapped));
            assert_eq!(seg(true, 0, whole).serviceable(op), Ok(()));
            assert_eq!(seg(false, 0, 4096).serviceable(op), Ok(()));
        }

        // Reads may take any piece of a page.
        assert_eq!(seg(true, 4096, 4096).serviceable(Op::Read), Ok(()));
    }
}
