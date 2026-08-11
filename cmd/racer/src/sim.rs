//! Deterministic simulation.
//!
//! The whole cluster runs in one thread. Every node is a real worker — same op slab,
//! pool and hop fabric — with only the kernel seams replaced: disk submission, the
//! timer, the clock, raw device IO and the guest copy. `server`, `paxos`, `alloc`,
//! `cache`, `heal` and `fabric` run unmodified above that line.
//!
//! Two substitutions carry the design. A device is a sparse block map, so a node's
//! store and its handle on a peer are the same kind of object. A buffer is ordinary
//! process memory, so a transfer is a memcpy the simulator does by address.

use std::cell::{Cell, RefCell};
use std::cmp::Reverse;
use std::collections::{BTreeMap, BTreeSet, BinaryHeap, VecDeque};
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::time::{Duration, Instant};

use crate::config::Config;
use crate::layout::{self, Class, Entry, Geometry, State};
use crate::runtime::{Buf, Durability, Errno, Op, Request, SimNode, SimWorker, sim_addr, sim_buf};
use crate::server::{self, SERVER};

/// Block size of every simulated device. Frames and mblocks are 4 KiB, so nothing
/// smaller is ever addressed.
const BLOCK: usize = 4096;
/// Nominal device size. Sparse, so only written blocks cost anything.
const DEVICE_BYTES: u64 = 64 << 30;
/// One-way message and disk latency, before jitter.
const LATENCY_US: u64 = 50;
/// What a straggling link adds one way. Long enough that it always loses a quorum
/// race, short enough that the frame still arrives before the link times out.
const SLOW_US: u64 = 200_000;
/// Virtual time between ticks. Matches the runtime's maintenance interval, so
/// `Handler::tick` keeps its cadence.
const TICK_US: u64 = 1_000;
/// Settle passes that may keep finding work before we call it a livelock.
const SETTLE_LIMIT: usize = 1 << 20;

// ---------------------------------------------------------------------------
// The ambient half: what handler code reaches through `crate::sim::*`
// ---------------------------------------------------------------------------

/// Which side of a transfer an op is. Mirrors the two `Disk` methods.
pub(crate) enum Kind {
    Read,
    #[allow(dead_code)]
    Write(Durability),
}

/// The op a completion belongs to. The slab recycles indices, so `seq` is part of the
/// name and a stale event fails to match.
#[derive(Copy, Clone, PartialEq, Eq, PartialOrd, Ord, Debug)]
struct Who {
    node: u32,
    core: u32,
    idx: u32,
    seq: u16,
}

/// One node's store, or a handle on a peer (which holds no blocks: `submit` turns
/// writes to it into frames). Absent blocks read as zero and all-zero writes are erased.
struct Device {
    node: u32,
    fabric: bool,
    blocks: BTreeMap<u64, Box<[u8; BLOCK]>>,
    /// Transfers submitted through the runtime, which are the ones a rate budget meters.
    ops: u64,
}

impl Device {
    fn read(&self, lba: u64, dst: &mut [u8]) {
        match self.blocks.get(&lba) {
            Some(b) => dst.copy_from_slice(&b[..]),
            None => dst.fill(0),
        }
    }

    fn write(&mut self, lba: u64, src: &[u8]) {
        if src.iter().all(|&b| b == 0) {
            self.blocks.remove(&lba);
            return;
        }
        let mut b = Box::new([0u8; BLOCK]);
        b.copy_from_slice(src);
        self.blocks.insert(lba, b);
    }
}

/// Everything a scheduled event can be. The simulator owns the workers, so an event
/// never touches one itself; it is popped and acted on by [`Sim::fire`].
enum What {
    /// A storage transfer, performed at completion time so overlapping IO to one block
    /// interleaves the way the device would.
    Disk { who: Who, dev: u32, read: bool, off: u64, addr: u64, len: u32 },
    /// A frame arriving at a peer. The payload is read out of the sender's buffer at
    /// delivery, not at submission, because that is when the transport reads it: a
    /// sender that recycles the buffer first puts whatever is there now on the wire.
    Send { from: Who, to: u32, read: bool, lba: u64, back: u64, len: u32, tries: u32 },
    /// A result travelling back to the op's submitter, with the payload for a read.
    Reply { who: Who, res: i32, back: u64, wire: Option<Vec<u8>> },
    /// A `sleep` expiring, or a link timeout firing.
    Wake { who: Who, res: i32 },
    /// Nothing but a step of virtual time, so an idle cluster still runs maintenance.
    Tick,
}

struct Ev {
    at: u64,
    seq: u64,
    what: What,
}

// Ordered by (time, submission order): a total order, so the heap's own tie-breaking
// cannot affect a run.
impl PartialEq for Ev {
    fn eq(&self, o: &Ev) -> bool {
        (self.at, self.seq) == (o.at, o.seq)
    }
}
impl Eq for Ev {}
impl PartialOrd for Ev {
    fn partial_cmp(&self, o: &Ev) -> Option<std::cmp::Ordering> {
        Some(self.cmp(o))
    }
}
impl Ord for Ev {
    fn cmp(&self, o: &Ev) -> std::cmp::Ordering {
        (self.at, self.seq).cmp(&(o.at, o.seq))
    }
}

/// Injected failure. Every rate is per mille of the operations it applies to.
#[derive(Clone, Default)]
pub struct Faults {
    /// A storage transfer fails with EIO.
    pub io_error: u32,
    /// A storage read flips a byte on the way back.
    pub corrupt: u32,
    /// A frame is lost. The sender is left to its link timeout.
    pub drop: u32,
    /// Extra one-way latency, uniform over `0..=jitter_us`.
    pub jitter_us: u64,
    /// Directed cuts. `(a, b)` present means nothing `a` sends reaches `b`.
    pub cut: BTreeSet<(u32, u32)>,
    /// Directed stragglers. `(a, b)` present means everything `a` sends to `b` still
    /// arrives, just far too late to win a quorum race.
    pub slow: BTreeSet<(u32, u32)>,
}

impl Faults {
    fn cut(&self, a: u32, b: u32) -> bool {
        self.cut.contains(&(a, b))
    }

    fn slow(&self, a: u32, b: u32) -> bool {
        self.slow.contains(&(a, b))
    }
}

/// State reachable from handler code, which cannot borrow the simulator itself: a
/// `Disk::read` happens while `Sim::run` is already on the stack.
struct Shared {
    now: Cell<u64>,
    /// Which (node, core) is executing, so a submission needs no arguments naming it.
    here: Cell<(u32, u32)>,
    devs: RefCell<Vec<Device>>,
    paths: RefCell<BTreeMap<PathBuf, u32>>,
    evs: RefCell<BinaryHeap<Reverse<Ev>>>,
    seq: Cell<u64>,
    /// Ops accepted and not yet completed. The slab must be told exactly once, so
    /// every completion path checks in here first.
    live: RefCell<BTreeSet<Who>>,
    /// Pieces still owed to a split transfer, with the length its one completion
    /// reports. Absent for the ordinary single-piece case.
    owed: RefCell<BTreeMap<Who, (u32, i32)>>,
    /// Every buffer the simulator hands to a request, by base address: its length and
    /// how many times that allocation has been handed out. A worker recycles a ublk
    /// tag's registered pages the moment the request completes, so a transfer that
    /// still reads or writes one after the count moved on is touching another
    /// request's memory.
    bufs: RefCell<BTreeMap<u64, (usize, u64)>>,
    /// The generation each live op was submitted against, so a late transfer can be
    /// told apart from a timely one.
    held: RefCell<BTreeMap<Who, u64>>,
    /// The peer's maximum data transfer size, or zero for a transport that never splits.
    mdts: Cell<u32>,
    rng: Cell<u64>,
    faults: RefCell<Faults>,
}

thread_local! {
    static SHARED: RefCell<Option<Rc<Shared>>> = const { RefCell::new(None) };
}

fn shared() -> Rc<Shared> {
    SHARED.with(|s| s.borrow().clone().expect("no simulation on this thread"))
}

impl Shared {
    fn rand(&self) -> u64 {
        // xorshift64*: the seed is the whole of a run's nondeterminism.
        let mut x = self.rng.get();
        x ^= x >> 12;
        x ^= x << 25;
        x ^= x >> 27;
        self.rng.set(x);
        x.wrapping_mul(0x2545_f491_4f6c_dd1d)
    }

    fn chance(&self, per_mille: u32) -> bool {
        per_mille > 0 && self.rand() % 1000 < per_mille as u64
    }

    fn latency(&self) -> u64 {
        let j = self.faults.borrow().jitter_us;
        LATENCY_US + if j == 0 { 0 } else { self.rand() % (j + 1) }
    }

    fn at(&self, delay_us: u64, what: What) {
        let seq = self.seq.get();
        self.seq.set(seq + 1);
        self.evs.borrow_mut().push(Reverse(Ev { at: self.now.get() + delay_us, seq, what }));
    }

    /// How many times the allocation holding `addr` has been handed out. Zero for
    /// memory the simulator did not lend, which is every buffer the node owns itself.
    fn generation(&self, addr: u64) -> u64 {
        let b = self.bufs.borrow();
        match b.range(..=addr).next_back() {
            Some((&base, &(len, g))) if addr < base + len as u64 => g,
            _ => 0,
        }
    }

    /// The invariant that makes an abandoned transfer visible: whatever it touches must
    /// still belong to the request that submitted it.
    fn still_ours(&self, who: &Who, addr: u64, what: &str) {
        let then = self.held.borrow().get(who).copied().unwrap_or(0);
        let now = self.generation(addr);
        assert_eq!(
            now, then,
            "{what} touches a recycled buffer: the request that submitted it has already \
             completed and its memory now serves another"
        );
    }
}

// ---------------------------------------------------------------------------
// The seams the runtime calls
// ---------------------------------------------------------------------------

/// Resolve a device path, creating it on first sight. The node id is carried in the
/// path, so nothing has to track which node is being built.
pub(crate) fn device(path: &Path) -> std::io::Result<u32> {
    let s = shared();
    if let Some(&id) = s.paths.borrow().get(path) {
        return Ok(id);
    }
    let text = path.to_string_lossy();
    let bad = || std::io::Error::from_raw_os_error(libc::ENOENT);
    let rest = text.strip_prefix("/sim/n").ok_or_else(bad)?;
    let (node, tail) = rest.split_once('/').ok_or_else(bad)?;
    let node: u32 = node.parse().map_err(|_| bad())?;
    let fabric = match tail {
        "store" => false,
        "fabric" => true,
        _ => return Err(bad()),
    };
    let mut devs = s.devs.borrow_mut();
    let id = devs.len() as u32;
    devs.push(Device { node, fabric, blocks: BTreeMap::new(), ops: 0 });
    s.paths.borrow_mut().insert(path.to_path_buf(), id);
    Ok(id)
}

pub(crate) fn device_bytes(_dev: u32) -> std::io::Result<u64> {
    Ok(DEVICE_BYTES)
}

/// Simulated time, for the few places that need a clock rather than a timer.
pub(crate) fn now_us() -> u64 {
    shared().now.get()
}

/// Raw device access, for `format` and the allocator's startup scan. Neither runs on a
/// worker, so neither goes through the op slab.
pub(crate) fn raw_read(dev: u32, off: u64, out: &mut [u8]) -> std::io::Result<()> {
    let s = shared();
    let devs = s.devs.borrow();
    for (i, chunk) in out.chunks_mut(BLOCK).enumerate() {
        devs[dev as usize].read(off / BLOCK as u64 + i as u64, chunk);
    }
    Ok(())
}

pub(crate) fn raw_write(dev: u32, off: u64, src: &[u8]) -> std::io::Result<()> {
    let s = shared();
    let mut devs = s.devs.borrow_mut();
    for (i, chunk) in src.chunks(BLOCK).enumerate() {
        devs[dev as usize].write(off / BLOCK as u64 + i as u64, chunk);
    }
    Ok(())
}

/// The guest copy. In production it crosses into the kernel; here both sides are ours.
pub(crate) fn copy_req(
    buf: Buf,
    off: usize,
    ptr: *mut u8,
    len: usize,
    store: bool,
) -> Result<(), Errno> {
    if off + len > buf.len() {
        return Err(Errno::EINVAL);
    }
    let base = (sim_addr(buf) as *mut u8).wrapping_add(off);
    // SAFETY: `off + len` is inside `buf` (checked above), and `buf` and `ptr` are
    // distinct live allocations.
    unsafe {
        if store {
            std::ptr::copy_nonoverlapping(ptr, base, len);
        } else {
            std::ptr::copy_nonoverlapping(base, ptr, len);
        }
    }
    Ok(())
}

/// A timer, as one event. `IORING_OP_TIMEOUT` reports `-ETIME` on expiry, but `sleep`
/// discards the result, so any value does.
pub(crate) fn sleep(d: Duration, idx: u32, seq: u16) {
    let s = shared();
    let (node, core) = s.here.get();
    let who = Who { node, core, idx, seq };
    s.live.borrow_mut().insert(who);
    s.at(d.as_micros() as u64, What::Wake { who, res: 0 });
}

/// A transfer. Storage goes to the device; a fabric device is a peer, so the frame is
/// scheduled for delivery and the reply comes back as a separate event.
pub(crate) fn submit(
    dev: u32,
    kind: Kind,
    off: u64,
    addr: u64,
    len: u32,
    timeout: Option<Duration>,
    idx: u32,
    seq: u16,
) {
    let s = shared();
    let (node, core) = s.here.get();
    let who = Who { node, core, idx, seq };
    s.live.borrow_mut().insert(who);
    s.held.borrow_mut().insert(who, s.generation(addr));
    let read = matches!(kind, Kind::Read);

    // The timeout is armed alongside: whichever event reaches the slab first wins, and
    // the other finds the op already gone.
    if let Some(t) = timeout {
        s.at(t.as_micros() as u64, What::Wake { who, res: -libc::ETIME });
    }

    let (peer, fabric) = {
        let mut devs = s.devs.borrow_mut();
        let d = &mut devs[dev as usize];
        d.ops += 1;
        (d.node, d.fabric)
    };
    let lat = s.latency();
    if fabric {
        if s.faults.borrow().cut(node, peer) {
            // Lost: only the timeout can finish this op now, which is the point.
            return;
        }
        let lat = lat + if s.faults.borrow().slow(node, peer) { SLOW_US } else { 0 };
        let lba = off / BLOCK as u64;
        // The transport splits anything above the peer's MDTS. The target sees the
        // pieces as separate requests at consecutive LBAs inside the frame's footprint,
        // and there is still one completion, so losing a piece loses the command.
        let mdts = match s.mdts.get() {
            0 => len,
            m => m,
        };
        let pieces = len.div_ceil(mdts);
        if pieces > 1 {
            s.owed.borrow_mut().insert(who, (pieces, len as i32));
        }
        for p in 0..pieces {
            let at = p * mdts;
            let n = mdts.min(len - at);
            let delay = if p == 0 { lat } else { s.latency() };
            if s.chance(s.faults.borrow().drop) {
                continue;
            }
            let e = What::Send {
                from: who,
                to: peer,
                read,
                lba: lba + (at / BLOCK as u32) as u64,
                back: addr + at as u64,
                len: n,
                tries: 0,
            };
            s.at(delay, e);
        }
    } else {
        s.at(lat, What::Disk { who, dev, read, off, addr, len });
    }
}

// ---------------------------------------------------------------------------
// The simulator
// ---------------------------------------------------------------------------

/// What a request slot is being used for, so a finished future can be routed.
enum Use {
    /// A client request, under the id `submit` returned.
    Client(u64),
    /// A frame from a peer, owed a reply. `wire` is the simulator's copy of the payload,
    /// and is what the handler runs against.
    Frame { origin: Who, read: bool, wire: Box<[u8]>, back: u64 },
}

struct NodeState {
    id: u32,
    cfg: Config,
    workers: Option<SimNode<server::Server>>,
    /// Held while the node is up; a restart builds a fresh one, re-reading the device.
    node: Option<Box<server::Node>>,
    dp: *const (),
    free: Vec<VecDeque<u32>>,
    slots: Vec<Vec<Option<Use>>>,
}

/// A client request's buffer, which must outlive the request, and the node serving it.
struct Pending {
    buf: Box<[u8]>,
    node: usize,
}

/// How a cluster is shaped. Groups, peers and volumes are all derived from these.
#[derive(Clone)]
pub struct Options {
    pub nodes: u32,
    pub cores: usize,
    pub seed: u64,
    /// 4 KiB pages in the small volume.
    pub pages: u64,
    /// 4 MiB pages in the immutable volume, or zero for none.
    pub huge_pages: u64,
    /// `τ`, the cache's target rate. Zero disables the cache entirely.
    pub cache_rate: u32,
    /// The rate the backing device is willing to be driven at, or zero for unmetered.
    pub device_iops: u64,
    /// Whether every node opens every other. `false` keeps only the nodes it shares a
    /// group with, which is what makes hundreds of nodes affordable.
    pub clique: bool,
    /// The peers' maximum data transfer size in bytes, or zero for a transport that
    /// delivers any transfer whole. Must be a multiple of the block size.
    pub mdts: u32,
    pub faults: Faults,
}

impl Default for Options {
    fn default() -> Options {
        Options {
            nodes: 3,
            cores: 1,
            seed: 1,
            pages: 4096,
            huge_pages: 0,
            cache_rate: 0,
            device_iops: 0,
            clique: true,
            mdts: 0,
            faults: Faults::default(),
        }
    }
}

/// Volume ids the simulator declares. Zero is the fabric and is reserved.
pub const SMALL: u64 = 1;
pub const HUGE: u64 = 2;

pub struct Sim {
    s: Rc<Shared>,
    base: Instant,
    opts: Options,
    nodes: Vec<NodeState>,
    pending: BTreeMap<u64, Pending>,
    results: BTreeMap<u64, Result<(), i32>>,
    next_id: u64,
    done: Vec<(u32, Result<(), Errno>)>,
    /// Retired request buffers, by length. A finished request's memory goes straight
    /// back into service, the way ublk re-arms a tag onto the next request's pages, so
    /// anything still pointing at it sees the next request's bytes rather than its own.
    /// Nothing is ever freed: an op that outlives its buffer is a bug, not a crash.
    spare: BTreeMap<usize, Vec<Box<[u8]>>>,
}

impl Sim {
    pub fn new(opts: Options) -> std::io::Result<Sim> {
        assert!(opts.nodes >= 3, "consensus needs three nodes");
        assert!((opts.mdts as usize).is_multiple_of(BLOCK), "mdts must be a whole number of blocks");
        raise_files();
        let s = Rc::new(Shared {
            now: Cell::new(0),
            here: Cell::new((0, 0)),
            devs: RefCell::new(Vec::new()),
            paths: RefCell::new(BTreeMap::new()),
            evs: RefCell::new(BinaryHeap::new()),
            seq: Cell::new(0),
            live: RefCell::new(BTreeSet::new()),
            owed: RefCell::new(BTreeMap::new()),
            bufs: RefCell::new(BTreeMap::new()),
            held: RefCell::new(BTreeMap::new()),
            mdts: Cell::new(opts.mdts),
            rng: Cell::new(opts.seed | 1),
            faults: RefCell::new(opts.faults.clone()),
        });
        SHARED.with(|c| *c.borrow_mut() = Some(s.clone()));
        s.at(TICK_US, What::Tick);

        let mut sim = Sim {
            s,
            base: Instant::now(),
            opts: opts.clone(),
            nodes: Vec::new(),
            pending: BTreeMap::new(),
            results: BTreeMap::new(),
            next_id: 1,
            done: Vec::new(),
            spare: BTreeMap::new(),
        };
        for i in 0..opts.nodes {
            let cfg = Config::parse(&sim.config_text(i + 1))?;
            cfg.validate()?;
            crate::layout::format(Path::new(&cfg.node.device), &cfg)?;
            sim.nodes.push(NodeState {
                id: i + 1,
                cfg,
                workers: None,
                node: None,
                dp: std::ptr::null(),
                free: Vec::new(),
                slots: Vec::new(),
            });
            sim.boot(i as usize)?;
        }
        Ok(sim)
    }

    // ---------------------------------------------------------------- topology

    /// Consensus groups: every window of three consecutive nodes, so each node sits in
    /// three groups and no group repeats a member.
    fn group(&self, g: u32) -> [u32; 3] {
        let n = self.opts.nodes;
        [g % n + 1, (g + 1) % n + 1, (g + 2) % n + 1]
    }

    fn peers_of(&self, id: u32) -> Vec<u32> {
        let n = self.opts.nodes;
        if self.opts.clique {
            return (1..=n).filter(|&p| p != id).collect();
        }
        // Only the nodes this one shares a group with: a clique is O(n²) links, and
        // every link is a registered device.
        let mut out = BTreeSet::new();
        for g in 0..n {
            let m = self.group(g);
            if m.contains(&id) {
                out.extend(m.iter().copied().filter(|&p| p != id));
            }
        }
        out.into_iter().collect()
    }

    fn config_text(&self, id: u32) -> String {
        let o = &self.opts;
        let mut t = String::new();
        t.push_str("generation 1\n");
        t.push_str(&format!(
            "node id={id} site=0 zone=1 cohort=0 device=/sim/n{id}/store cache_4k=0 cache_4m=0 max_iops={}\n",
            o.device_iops
        ));
        for p in self.peers_of(id) {
            t.push_str(&format!("peer id={p} device=/sim/n{p}/fabric\n"));
        }
        t.push_str("topology epoch=1\n");
        for g in 0..o.nodes {
            let m = self.group(g);
            t.push_str(&format!("group {} {} {}\n", m[0], m[1], m[2]));
        }
        t.push_str("slots round_robin\n");
        // The index ceiling is a real check; give it room for the volume we declare.
        let idx = o.pages * crate::alloc::INDEX_BYTES_PER_PAGE + (1 << 20);
        t.push_str(&format!(
            "policy max_index_bytes={idx} occ_bytes={} cache_target_rate={}\n",
            1 << 20,
            o.cache_rate
        ));
        t.push_str(&format!("volume {SMALL} slot=1\n"));
        t.push_str(&format!("extent pages={} kind=lww zone=1\n", o.pages));
        if o.huge_pages > 0 {
            t.push_str(&format!("volume {HUGE} slot=2\n"));
            t.push_str(&format!("extent pages={} kind=immutable_4m zone=1\n", o.huge_pages));
        }
        t
    }

    // ---------------------------------------------------------------- lifecycle

    /// Bring a node up against whatever its device already holds. Restart is the same
    /// call, which is the whole of the recovery test.
    fn boot(&mut self, i: usize) -> std::io::Result<()> {
        let cores = self.opts.cores;
        let cfg = self.nodes[i].cfg.clone();
        let workers = SimNode::<server::Server>::new(cores, &SERVER, self.base)?;
        let node = Box::new(server::Node::new());
        let cfgr = crate::runtime::Configurator::sim(cores);
        // `attach` runs on the control thread in production and touches no worker
        // state, so it needs no `Local`.
        let dp: *const () = Box::leak(Box::new(node.attach(&cfgr, cfg)?)) as *const _ as *const ();
        let n = &mut self.nodes[i];
        for c in 0..cores {
            workers.at(c).publish(1, dp);
        }
        n.free = (0..cores).map(|_| (0..slots_per_worker()).collect()).collect();
        n.slots = (0..cores).map(|_| (0..slots_per_worker()).map(|_| None).collect()).collect();
        n.workers = Some(workers);
        n.node = Some(node);
        n.dp = dp;
        Ok(())
    }

    /// Lose a node outright: its workers, its in-flight work and everything it had not
    /// written down. The device survives, so what comes back is exactly what was durable.
    pub fn crash(&mut self, i: usize) {
        let id = self.nodes[i].id;
        self.nodes[i].workers = None;
        self.nodes[i].node = None;
        self.nodes[i].free.clear();
        // Its in-flight frame buffers go back into service rather than away: a scheduled
        // event may still name one, and reading recycled memory is the honest outcome.
        for core in std::mem::take(&mut self.nodes[i].slots) {
            for u in core.into_iter().flatten() {
                if let Use::Frame { wire, .. } = u {
                    self.put_buf(wire);
                }
            }
        }
        // Forget its live ops: a restart hands out the same (idx, seq) again, and a
        // stale event must not complete a fresh op.
        self.s.live.borrow_mut().retain(|w| w.node != id);
        self.s.owed.borrow_mut().retain(|w, _| w.node != id);
        self.s.held.borrow_mut().retain(|w, _| w.node != id);
        // Its in-flight client requests can never complete, so fail them now.
        let lost: Vec<u64> =
            self.pending.iter().filter(|(_, p)| p.node == i).map(|(&k, _)| k).collect();
        for k in lost {
            if let Some(p) = self.pending.remove(&k) {
                self.put_buf(p.buf);
            }
            self.results.insert(k, Err(libc::EIO));
        }
    }

    pub fn restart(&mut self, i: usize) -> std::io::Result<()> {
        assert!(self.nodes[i].workers.is_none(), "node is already up");
        self.boot(i)
    }

    pub fn up(&self, i: usize) -> bool {
        self.nodes[i].workers.is_some()
    }

    pub fn nodes(&self) -> usize {
        self.nodes.len()
    }

    /// Cut or restore one direction between two nodes.
    pub fn cut(&mut self, a: usize, b: usize, on: bool) {
        let (a, b) = (self.nodes[a].id, self.nodes[b].id);
        let mut f = self.s.faults.borrow_mut();
        if on {
            f.cut.insert((a, b));
        } else {
            f.cut.remove(&(a, b));
        }
    }

    /// Replace the fault profile mid-run; how a test quiesces before checking liveness.
    pub fn faults(&mut self, f: Faults) {
        *self.s.faults.borrow_mut() = f;
    }

    pub fn now(&self) -> Duration {
        Duration::from_micros(self.s.now.get())
    }

    /// Transfers a node has submitted to its own store since boot. Counts what a rate
    /// budget meters, so a test can check the budget was actually held to.
    pub fn device_ops(&self, node: usize) -> u64 {
        let path = Path::new(&self.nodes[node].cfg.node.device);
        let dev = *self.s.paths.borrow().get(path).expect("store device");
        self.s.devs.borrow()[dev as usize].ops
    }

    /// Damage the persisted bytes of one replica of a small page. `replica` is a
    /// position in the page's consensus group; the return value is that node's index.
    pub fn corrupt_small_replica(&mut self, lba: u64, replica: usize) -> usize {
        let (node, geo, slot, _) = self
            .small_replica_location(lba, replica)
            .unwrap_or_else(|| panic!("small page {lba} is not live on replica {replica}"));
        let path = Path::new(&self.nodes[node].cfg.node.device);
        let dev = *self.s.paths.borrow().get(path).expect("store device");
        let off = geo.slot_off(Class::Small, slot);
        let mut devs = self.s.devs.borrow_mut();
        let d = &mut devs[dev as usize];
        let mut block = [0u8; BLOCK];
        d.read(off / BLOCK as u64, &mut block);
        block[17] ^= 0xff;
        d.write(off / BLOCK as u64, &block);
        node
    }

    /// Whether a replica's persisted page still matches its mblock entry's `data_crc`.
    pub fn small_replica_valid(&self, lba: u64, replica: usize) -> bool {
        let Some((node, geo, slot, entry)) = self.small_replica_location(lba, replica) else {
            return false;
        };
        let path = Path::new(&self.nodes[node].cfg.node.device);
        let Some(&dev) = self.s.paths.borrow().get(path) else { return false };
        let mut page = [0u8; BLOCK];
        self.s.devs.borrow()[dev as usize].read(
            geo.slot_off(Class::Small, slot) / BLOCK as u64,
            &mut page,
        );
        layout::page_crc(entry.addr, entry.version, &page) == entry.data_crc
    }

    fn small_replica_location(
        &self,
        lba: u64,
        replica: usize,
    ) -> Option<(usize, Geometry, u32, Entry)> {
        let addr = crate::alloc::GlobalAddr::new(SMALL as u32, u32::try_from(lba).ok()?);
        let cfg = &self.nodes.first()?.cfg;
        let group = cfg.group(addr.0) as usize;
        let member = *cfg.topology.catalog.get(group)?.get(replica)?;
        let node = self.nodes.iter().position(|n| n.id == member)?;
        let path = Path::new(&self.nodes[node].cfg.node.device);
        let geo = layout::read_geometry(path).ok()?;
        let dev = *self.s.paths.borrow().get(path)?;
        let mut raw = [0u8; BLOCK];

        for id in 0..geo.mblocks(Class::Small) as u32 {
            let mut best: Option<(u64, [u8; BLOCK])> = None;
            for copy in 0..2u8 {
                raw_read(dev, geo.mblock_off(Class::Small, id, copy), &mut raw).ok()?;
                let Some(h) = layout::get_header(&raw) else { continue };
                if h.class != Class::Small || h.mblock_id != id {
                    continue;
                }
                if best.as_ref().is_none_or(|(generation, _)| h.generation > *generation) {
                    best = Some((h.generation, raw));
                }
            }
            let Some((_, block)) = best else { continue };
            for i in 0..Class::Small.k() {
                let entry = layout::get_entry(&block, Class::Small, i);
                if entry.addr == addr.0 && entry.state == State::Live {
                    return Some((node, geo, id * Class::Small.k() + i, entry));
                }
            }
        }
        None
    }

    // ---------------------------------------------------------------- client IO

    /// A buffer to serve a request out of, poisoned so that reading one before it has
    /// been filled, or after it has been handed on, is visible rather than plausible.
    fn take_buf(&mut self, len: usize) -> Box<[u8]> {
        let mut b = match self.spare.get_mut(&len).and_then(|v| v.pop()) {
            Some(mut b) => {
                b.fill(0xa5);
                b
            }
            None => vec![0xa5u8; len].into_boxed_slice(),
        };
        // Handing it out is what invalidates whatever still points at it.
        let base = b.as_mut_ptr() as u64;
        let mut bufs = self.s.bufs.borrow_mut();
        let e = bufs.entry(base).or_insert((len, 0));
        *e = (len, e.1 + 1);
        drop(bufs);
        b
    }

    fn put_buf(&mut self, b: Box<[u8]>) {
        self.spare.entry(b.len()).or_default().push(b);
    }

    /// Issue a request against a node. Returns the id its result will arrive under.
    fn submit(&mut self, i: usize, vol: u64, op: Op, lba: u64, data: Option<&[u8]>) -> u64 {
        let id = self.next_id;
        self.next_id += 1;
        let huge = vol == HUGE;
        let len = if huge { 4 << 20 } else { BLOCK };
        let mut buf = self.take_buf(len);
        if let Some(d) = data {
            buf[..d.len()].copy_from_slice(d);
        }
        let Some(core) = self.pick(i, lba) else {
            self.put_buf(buf);
            self.results.insert(id, Err(libc::EIO));
            return id;
        };
        let b = sim_buf(0, buf.as_ptr() as u64, len as u32);
        self.pending.insert(id, Pending { buf, node: i });
        self.start(i, core, Use::Client(id), Request { vol, op, lba, buf: b });
        id
    }

    pub fn write(&mut self, i: usize, lba: u64, fill: u8) -> u64 {
        let page = vec![fill; BLOCK];
        self.submit(i, SMALL, Op::Write, lba, Some(&page))
    }

    pub fn read(&mut self, i: usize, lba: u64) -> u64 {
        self.submit(i, SMALL, Op::Read, lba, None)
    }

    pub fn trim(&mut self, i: usize, lba: u64) -> u64 {
        self.submit(i, SMALL, Op::Discard, lba, None)
    }

    /// Huge pages are addressed by page: one request is one whole 4 MiB page, the only
    /// shape the immutable class accepts.
    pub fn write_huge(&mut self, i: usize, page: u64, fill: u8) -> u64 {
        let buf = vec![fill; 4 << 20];
        self.submit(i, HUGE, Op::Write, page * 1024, Some(&buf))
    }

    pub fn read_huge(&mut self, i: usize, page: u64) -> u64 {
        self.submit(i, HUGE, Op::Read, page * 1024, None)
    }

    pub fn result(&self, id: u64) -> Option<Result<(), i32>> {
        self.results.get(&id).copied()
    }

    /// The bytes a finished read returned. `None` once its node has crashed.
    pub fn payload(&self, id: u64) -> Option<&[u8]> {
        self.pending.get(&id).map(|p| &p.buf[..])
    }

    /// A live worker on `i`, chosen by address so a page always lands on the same one.
    fn pick(&self, i: usize, lba: u64) -> Option<usize> {
        let n = self.nodes.get(i)?;
        let w = n.workers.as_ref()?;
        Some((lba as usize) % w.cores())
    }

    /// Hand a request to a worker, recording what its slot is owed to. Returns false
    /// if the worker has no slot free.
    fn start(&mut self, i: usize, core: usize, u: Use, req: Request) -> bool {
        let Some(slot) = self.nodes[i].free[core].pop_front() else {
            match u {
                Use::Client(id) => {
                    self.results.insert(id, Err(libc::EAGAIN));
                }
                Use::Frame { origin, back, wire, .. } => {
                    let me = self.nodes[i].id;
                    self.put_buf(wire);
                    self.reply(me, origin, -libc::EIO, back, None)
                }
            }
            return false;
        };
        self.nodes[i].slots[core][slot as usize] = Some(u);
        // The first poll happens inside `start` below, so the ambient identity must be
        // right before it: an op submitted during that poll names this worker.
        self.s.here.set((self.nodes[i].id, core as u32));
        let w = self.nodes[i].workers.as_mut().unwrap().worker(core);
        w.enter();
        let imm = w.start(slot, req);
        SimWorker::<server::Server>::leave();
        if let Some(res) = imm {
            self.finish(i, core, slot, res);
        }
        true
    }

    /// Route a finished request and give its slot back.
    fn finish(&mut self, i: usize, core: usize, slot: u32, res: Result<(), Errno>) {
        let u = self.nodes[i].slots[core][slot as usize].take();
        self.nodes[i].free[core].push_back(slot);
        match u {
            Some(Use::Client(id)) => {
                self.results.insert(id, res.map_err(|e| e.raw()));
                // The tag is committed and re-armed here, so the request's memory goes
                // back into service. What it returned is kept as a copy for `payload`.
                if let Some(p) = self.pending.get_mut(&id) {
                    let copy = p.buf.to_vec().into_boxed_slice();
                    let old = std::mem::replace(&mut p.buf, copy);
                    self.put_buf(old);
                }
            }
            Some(Use::Frame { origin, read, wire, back }) => {
                let r = match res {
                    Ok(()) => wire.len() as i32,
                    Err(e) => -e.raw(),
                };
                let me = self.nodes[i].id;
                // Only a read carries bytes home. A write's reply is a status; copying
                // into the sender's buffer would invent a transfer the wire never made.
                let back_wire = (read && r >= 0).then(|| wire.to_vec());
                self.put_buf(wire);
                self.reply(me, origin, r, back, back_wire);
            }
            None => unreachable!("finished a slot nobody claimed"),
        }
    }

    /// Send a status back. The return path is cut separately, which is what makes an
    /// asymmetric partition expressible.
    fn reply(&self, from: u32, origin: Who, res: i32, back: u64, wire: Option<Vec<u8>>) {
        if self.s.faults.borrow().cut(from, origin.node) {
            return;
        }
        let lat = self.s.latency();
        self.s.at(lat, What::Reply { who: origin, res, back, wire });
    }

    // ---------------------------------------------------------------- the loop

    /// Advance virtual time by `d`, running everything that falls inside it.
    pub fn run(&mut self, d: Duration) {
        let deadline = self.s.now.get() + d.as_micros() as u64;
        loop {
            self.settle();
            let next = self.s.evs.borrow().peek().map(|Reverse(e)| e.at);
            match next {
                Some(t) if t <= deadline => {
                    self.s.now.set(t.max(self.s.now.get()));
                    let ev = self.s.evs.borrow_mut().pop().unwrap().0;
                    self.fire(ev.what);
                }
                _ => break,
            }
        }
        self.s.now.set(deadline.max(self.s.now.get()));
        self.settle();
    }

    /// Run every worker until none of them has anything left to do at this instant.
    fn settle(&mut self) {
        let clock = self.base + Duration::from_micros(self.s.now.get());
        let mut done = std::mem::take(&mut self.done);
        let mut spins = 0;
        loop {
            let mut work = 0;
            for i in 0..self.nodes.len() {
                let cores = match self.nodes[i].workers.as_ref() {
                    Some(w) => w.cores(),
                    None => continue,
                };
                for core in 0..cores {
                    self.s.here.set((self.nodes[i].id, core as u32));
                    done.clear();
                    {
                        let w = self.nodes[i].workers.as_mut().unwrap().worker(core);
                        w.enter();
                        work += w.step(clock, &mut done);
                        SimWorker::<server::Server>::leave();
                    }
                    for (slot, res) in done.drain(..) {
                        self.finish(i, core, slot, res);
                    }
                }
            }
            if work == 0 {
                break;
            }
            spins += 1;
            assert!(spins < SETTLE_LIMIT, "workers never went idle: livelock");
        }
        self.done = done;
    }

    /// Claim an op, so it is completed exactly once. A late timeout, or a reply that
    /// lost a race with one, finds it gone.
    fn take(&self, who: Who) -> bool {
        let took = self.s.live.borrow_mut().remove(&who);
        if took {
            self.s.owed.borrow_mut().remove(&who);
            self.s.held.borrow_mut().remove(&who);
        }
        took
    }

    fn complete(&mut self, who: Who, res: i32) {
        let Some(i) = self.nodes.iter().position(|n| n.id == who.node) else { return };
        let Some(w) = self.nodes[i].workers.as_mut() else { return };
        if who.core as usize >= w.cores() {
            return;
        }
        self.s.here.set((who.node, who.core));
        let w = w.worker(who.core as usize);
        w.enter();
        crate::runtime::sim_complete(who.idx, who.seq, res);
        SimWorker::<server::Server>::leave();
    }

    fn fire(&mut self, what: What) {
        match what {
            What::Tick => self.s.at(TICK_US, What::Tick),
            What::Wake { who, res } => {
                if self.take(who) {
                    self.complete(who, res);
                }
            }
            What::Reply { who, res, back, wire } => {
                if !self.s.live.borrow().contains(&who) {
                    return;
                }
                // SAFETY: the op was still live, so the buffer it named is alive.
                if let Some(w) = wire {
                    self.s.still_ours(&who, back, "a reply");
                    let dst = unsafe { std::slice::from_raw_parts_mut(back as *mut u8, w.len()) };
                    dst.copy_from_slice(&w);
                }
                // A split transfer still has one completion: the last piece home ends
                // it, and any failed piece ends it early with that failure.
                let whole = if res < 0 {
                    Some(res)
                } else {
                    let mut owed = self.s.owed.borrow_mut();
                    match owed.get_mut(&who) {
                        Some((left, total)) => {
                            *left -= 1;
                            (*left == 0).then_some(*total)
                        }
                        None => Some(res),
                    }
                };
                if let Some(res) = whole
                    && self.take(who)
                {
                    self.complete(who, res);
                }
            }
            What::Disk { who, dev, read, off, addr, len } => {
                if !self.s.live.borrow().contains(&who) {
                    return;
                }
                self.s.still_ours(&who, addr, "a storage transfer");
                self.take(who);
                let res = self.transfer(dev, read, off, addr, len);
                self.complete(who, res);
            }
            What::Send { from, to, read, lba, back, len, tries } => {
                self.deliver(from, to, read, lba, back, len, tries)
            }
        }
    }

    /// Perform a storage transfer, injecting failure at the point the device would
    /// have reported it.
    fn transfer(&self, dev: u32, read: bool, off: u64, addr: u64, len: u32) -> i32 {
        let (bad, rot) = {
            let f = self.s.faults.borrow();
            (f.io_error, f.corrupt)
        };
        if self.s.chance(bad) {
            return -libc::EIO;
        }
        let n = len as usize;
        // SAFETY: the op is live, so `addr` names a buffer of ours that is still alive.
        let mem = unsafe { std::slice::from_raw_parts_mut(addr as *mut u8, n) };
        let base = off / BLOCK as u64;
        let mut devs = self.s.devs.borrow_mut();
        let d = &mut devs[dev as usize];
        for (i, chunk) in mem.chunks_mut(BLOCK).enumerate() {
            if read {
                d.read(base + i as u64, chunk);
            } else {
                d.write(base + i as u64, chunk);
            }
        }
        if read && self.s.chance(rot) {
            // Silent corruption on the way back: caught by the small class's page
            // checksum and, by design, not by the huge class's.
            let at = (self.s.rand() as usize) % n;
            mem[at] ^= 0xff;
        }
        len as i32
    }

    /// Deliver a frame. The peer runs the real handler against the simulator's copy of
    /// the payload, so neither end can see the other's buffer except through the reply.
    #[allow(clippy::too_many_arguments)]
    fn deliver(&mut self, from: Who, to: u32, read: bool, lba: u64, back: u64, len: u32, tries: u32) {
        // The command was cancelled — by its link timeout, or because its node went
        // away. A cancelled command's buffer is the initiator's again, so nothing may
        // be read out of it.
        if !self.s.live.borrow().contains(&from) {
            return;
        }
        let Some(i) = self.nodes.iter().position(|n| n.id == to) else { return };
        if self.nodes[i].workers.is_none() {
            return; // down; the sender's link timeout answers for it
        }
        let core = match self.pick(i, lba) {
            Some(c) => c,
            None => return,
        };
        if self.nodes[i].free[core].is_empty() {
            // The peer is saturated. Retry, but not forever: past this the sender's
            // timeout is the honest answer.
            if tries < 64 {
                let e = What::Send { from, to, read, lba, back, len, tries: tries + 1 };
                self.s.at(LATENCY_US, e);
            }
            return;
        }
        let op = if read { Op::Read } else { Op::Write };
        // The wire is read out of the sender's buffer now, at the instant the transport
        // would have.
        self.s.still_ours(&from, back, "a frame");
        let mut wire = self.take_buf(len as usize);
        // SAFETY: the op is live, so `back` names a buffer of ours that is still alive.
        wire.copy_from_slice(unsafe { std::slice::from_raw_parts(back as *const u8, len as usize) });
        let b = sim_buf(0, wire.as_mut_ptr() as u64, len);
        let u = Use::Frame { origin: from, read, wire, back };
        self.start(i, core, u, Request { vol: 0, op, lba, buf: b });
    }
}

/// Every simulated worker owns a real io_uring fd, so a few hundred nodes need more
/// descriptors than the default soft limit allows.
fn raise_files() {
    unsafe {
        let mut r: libc::rlimit = std::mem::zeroed();
        if libc::getrlimit(libc::RLIMIT_NOFILE, &mut r) == 0 && r.rlim_cur < r.rlim_max {
            r.rlim_cur = r.rlim_max;
            libc::setrlimit(libc::RLIMIT_NOFILE, &r);
        }
    }
}

/// One request slot per (device, tag) the runtime would have armed. The simulator
/// hands them out itself, so this is only a ceiling.
fn slots_per_worker() -> u32 {
    crate::runtime::sim_slots()
}

impl Drop for Sim {
    fn drop(&mut self) {
        // Workers must die while the simulation they reach into still exists.
        for n in &mut self.nodes {
            n.workers = None;
        }
        SHARED.with(|c| *c.borrow_mut() = None);
    }
}
