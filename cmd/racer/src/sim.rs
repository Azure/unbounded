//! Deterministic simulation.
//!
//! The whole cluster runs in one thread. Every node is a real worker; only the kernel
//! seams are replaced: disk submission, timer, clock, raw device IO and the guest copy.
//! `server`, `paxos`, `alloc`, `cache`, `heal` and `fabric` run unmodified. A device is a
//! sparse block map, so a node's store and its handle on a peer are the same object. A
//! buffer is process memory, so a transfer is a memcpy done by address.

use std::cell::{Cell, RefCell};
use std::cmp::Reverse;
use std::collections::{BTreeMap, BTreeSet, BinaryHeap, VecDeque};
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::time::{Duration, Instant};

use crate::config::Config;
use crate::layout::{self, Class, Entry, Geometry, State};
use crate::runtime::{Buf, Errno, Handler, Op, Request, SimNode, SimWorker, sim_addr, sim_buf};
use crate::server::{self, SERVER};

/// Block size of every simulated device. Frames and mblocks are 4 KiB.
const BLOCK: usize = 4096;
/// Chunks left past the layout for the cache to claim. The cache is entitled to the whole
/// tail, so this is the only thing bounding the media it holds.
const TAIL_CHUNKS: u64 = 16;
/// Cache slot records a node may pay for, in bytes. The cache opens holding a sixty-fourth
/// of what this buys, which is one 4 MiB chunk of 4 KiB slots: enough that the whole small
/// extent can sit in cache, which is what the tests are here to exercise. The default is a
/// gigabyte, sized for a real node; a campaign runs a whole cluster per seed and cannot
/// afford a production cache index on every one of them.
const CACHE_INDEX_BYTES: u64 = (1 << 16) * crate::cache::BYTES_PER_SLOT;

/// Store size a node asks for: the layout it is about to declare, plus a tail to cache in.
///
/// Sparse, so unwritten blocks cost nothing on the host, but the cache is entitled to
/// everything the layout does not claim, and a slot record is real memory. A store sized
/// like a real one would give every simulated node a production cache index, and the
/// campaign runs as many nodes at once as it has seeds in flight.
fn store_bytes(o: &Options) -> u64 {
    let floor = layout::store_floor(o.pages, o.huge_pages);

    floor.next_multiple_of(layout::CHUNK_BYTES) + TAIL_CHUNKS * layout::CHUNK_BYTES
}
/// One-way message and disk latency, before jitter.
const LATENCY_US: u64 = 50;
/// Delay a straggler adds one way: loses every quorum race, still beats the link timeout.
const SLOW_US: u64 = 200_000;
/// Virtual time between ticks, matching the runtime interval that paces `Handler::tick`.
const TICK_US: u64 = 1_000;
/// Settle passes that may keep finding work before we call it a livelock.
const SETTLE_LIMIT: usize = 1 << 20;

/// A path through the system worth proving a campaign reached.
///
/// A fuzzer that stopped splitting transfers, or stopped losing a frame, would go on
/// passing every invariant while testing less and less. These counters are what a
/// campaign asserts against, so a branch falling out of reach is a failure rather than a
/// silent loss of coverage.
#[derive(Copy, Clone, PartialEq, Eq, PartialOrd, Ord, Debug)]
pub enum Hit {
    /// A frame piece was dropped on the way out.
    Drop,
    /// A storage transfer was failed with EIO.
    IoError,
    /// A storage read flipped a byte on the way back.
    Corrupt,
    /// A command was refused at submission because its link was cut.
    CutSubmit,
    /// A frame in flight was discarded because its link was cut under it.
    CutDeliver,
    /// A reply was discarded because the return path was cut.
    CutReply,
    /// A frame took a straggler's path.
    Slow,
    /// A command was split by the peer's MDTS.
    Split,
    /// A piece of a split command was delivered.
    Piece,
    /// A frame found the target with no request slot free.
    Saturated,
    /// A `WARM` frame was delivered.
    Warm,
    /// A frame crossed a zone boundary.
    Crossing,
    /// A node was crashed.
    Crash,
    /// A node was restarted.
    Restart,
}

impl Hit {
    /// Every path, in order, so a campaign can name the one it never reached.
    pub const ALL: [Hit; 14] = [
        Hit::Drop,
        Hit::IoError,
        Hit::Corrupt,
        Hit::CutSubmit,
        Hit::CutDeliver,
        Hit::CutReply,
        Hit::Slow,
        Hit::Split,
        Hit::Piece,
        Hit::Saturated,
        Hit::Warm,
        Hit::Crossing,
        Hit::Crash,
        Hit::Restart,
    ];

    pub fn name(self) -> &'static str {
        match self {
            Hit::Drop => "a frame was dropped",
            Hit::IoError => "a storage transfer failed",
            Hit::Corrupt => "a storage read was corrupted",
            Hit::CutSubmit => "a command met a cut link",
            Hit::CutDeliver => "a frame met a cut link",
            Hit::CutReply => "a reply met a cut link",
            Hit::Slow => "a frame took a straggler's path",
            Hit::Split => "a command was split by MDTS",
            Hit::Piece => "a piece of a split command arrived",
            Hit::Saturated => "a target had no slot free",
            Hit::Warm => "a warm frame arrived",
            Hit::Crossing => "a frame crossed a zone",
            Hit::Crash => "a node crashed",
            Hit::Restart => "a node restarted",
        }
    }
}

// --- the ambient half: what handler code reaches through `crate::sim::*` ---

/// Which side of a transfer an op is. Mirrors the two `Disk` methods.
pub(crate) enum Kind {
    Read,
    Write,
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

/// One 4 KiB block's bytes, shared by every store holding that content.
type Block = Rc<[u8; BLOCK]>;

/// How many interns pass between sweeps of the block pool. Big enough that the walk is
/// amortised over the writes that caused it, small enough that a run overwriting one page
/// in a tight loop holds only a bounded number of the versions it has retired.
const SWEEP: u32 = 4096;

/// The bytes every simulated store is made of, interned by content.
///
/// A campaign writes each page to three replicas, warms it into another zone and re-sends
/// it on every retry, and an immutable page is written once and then read for the rest of
/// the run. A copy per node per write costs gigabytes over a long campaign and says
/// nothing the content does not, so a block is stored once and pointed at: the simulation
/// costs what it wrote, not what it copied. Sharing is safe because a stored block is
/// never edited in place; a write replaces the pointer.
#[derive(Default)]
struct Pool {
    /// Blocks by content hash. A bucket holds the few blocks that collided, compared in
    /// full before one is reused, so a collision costs a memcmp and never a wrong read.
    by_hash: BTreeMap<u64, Vec<Block>>,
    /// Interns since the last sweep.
    since: u32,
}

impl Pool {
    fn intern(&mut self, src: &[u8]) -> Block {
        self.since += 1;
        if self.since >= SWEEP {
            self.sweep();
        }
        let v = self.by_hash.entry(spread(src)).or_default();
        if let Some(b) = v.iter().find(|b| b[..] == *src) {
            return Rc::clone(b);
        }
        let mut b = [0u8; BLOCK];
        b.copy_from_slice(src);
        let b: Block = Rc::new(b);
        v.push(Rc::clone(&b));
        b
    }

    /// Drop the blocks no store points at any more. The pool's own reference is the one
    /// that has to be discounted, so a count of one means retired.
    fn sweep(&mut self) {
        self.since = 0;
        self.by_hash.retain(|_, v| {
            v.retain(|b| Rc::strong_count(b) > 1);
            !v.is_empty()
        });
    }
}

/// FNV-1a a word at a time over a block. Not a checksum: it only has to spread contents
/// over buckets, and a collision is compared away rather than believed.
fn spread(b: &[u8]) -> u64 {
    let mut h = 0xcbf2_9ce4_8422_2325u64;
    for w in b.chunks_exact(8) {
        h = (h ^ u64::from_le_bytes(w.try_into().unwrap())).wrapping_mul(0x0100_0000_01b3);
    }
    h
}

/// One node's store, or a handle on a peer (which holds no blocks: `submit` turns writes
/// to it into frames). Absent blocks read as zero and all-zero writes are erased.
struct Device {
    node: u32,
    fabric: bool,
    blocks: BTreeMap<u64, Block>,
    /// Store size, zero until `resize`. Unused on a fabric handle.
    len: u64,
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

    fn write(&mut self, pool: &mut Pool, lba: u64, src: &[u8]) {
        if src.iter().all(|&b| b == 0) {
            self.blocks.remove(&lba);
            return;
        }
        self.blocks.insert(lba, pool.intern(src));
    }
}

/// Everything a scheduled event can be. Events are popped and acted on by [`Sim::fire`].
enum What {
    /// A storage transfer, performed at completion time so overlapping IO interleaves.
    Disk {
        who: Who,
        dev: u32,
        read: bool,
        off: u64,
        addr: u64,
        len: u32,
    },
    /// A frame at a peer. Payload is read from the sender's buffer at delivery, not
    /// submission, which is when the transport reads it.
    Send {
        from: Who,
        to: u32,
        read: bool,
        lba: u64,
        back: u64,
        len: u32,
        tries: u32,
    },
    /// A result travelling back to the op's submitter, with the payload for a read.
    Reply {
        from: u32,
        who: Who,
        res: i32,
        back: u64,
        wire: Option<Vec<u8>>,
    },
    /// A `sleep` expiring, or a link timeout firing.
    Wake { who: Who, res: i32 },
    /// A step of virtual time, so an idle cluster still runs maintenance.
    Tick,
}

struct Ev {
    at: u64,
    seq: u64,
    what: What,
}

// Ordered by (time, submission order): total, so heap tie-breaking cannot affect a run.
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
    /// Directed stragglers. `(a, b)` present means everything `a` sends to `b` arrives
    /// too late to win a quorum race.
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
    /// The host instant virtual time is measured from. Every clock the runtime reads is
    /// this plus `now`, so nothing in the system can tell the two apart.
    base: Instant,
    /// Which (node, core) is executing, so a submission needs no arguments naming it.
    here: Cell<(u32, u32)>,
    devs: RefCell<Vec<Device>>,
    /// The block contents every store in the simulation shares.
    pool: RefCell<Pool>,
    paths: RefCell<BTreeMap<PathBuf, u32>>,
    evs: RefCell<BinaryHeap<Reverse<Ev>>>,
    seq: Cell<u64>,
    /// Ops accepted and not yet completed. The slab must be told exactly once, so every
    /// completion path checks in here first.
    live: RefCell<BTreeSet<Who>>,
    /// Pieces still owed to a split transfer, with the length its one completion reports.
    /// Absent for the ordinary single-piece case.
    owed: RefCell<BTreeMap<Who, (u32, i32)>>,
    /// Every buffer the simulator hands to a request, by base address: its length and how
    /// many times that allocation has been handed out. A worker recycles a ublk tag's
    /// registered pages as soon as the request completes, so a transfer touching one
    /// after the count moved on is touching another request's memory.
    bufs: RefCell<BTreeMap<u64, (usize, u64)>>,
    /// The generation each live op was submitted against, so a late transfer can be told
    /// apart from a timely one.
    held: RefCell<BTreeMap<Who, u64>>,
    /// The peer's maximum data transfer size, or zero for a transport that never splits.
    mdts: Cell<u32>,
    rng: Cell<u64>,
    faults: RefCell<Faults>,
    /// How many times each [`Hit`] has been reached, so a campaign can prove it got
    /// there. Counted here rather than in the tests because most of these are decisions
    /// only the simulator sees.
    hits: Cell<[u64; Hit::ALL.len()]>,
}

thread_local! {
    static SHARED: RefCell<Option<Rc<Shared>>> = const { RefCell::new(None) };
}

fn shared() -> Rc<Shared> {
    SHARED.with(|s| s.borrow().clone().expect("no simulation on this thread"))
}

impl Shared {
    /// Record that a path was taken. Saturating, since a campaign only ever asks whether
    /// a count is non-zero or how one compares with another.
    fn hit(&self, h: Hit) {
        let mut c = self.hits.get();
        c[h as usize] = c[h as usize].saturating_add(1);
        self.hits.set(c);
    }

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
        self.evs.borrow_mut().push(Reverse(Ev {
            at: self.now.get() + delay_us,
            seq,
            what,
        }));
    }

    /// How many times the allocation holding `addr` has been handed out. Zero for memory
    /// the simulator did not lend, which is every buffer the node owns itself.
    fn generation(&self, addr: u64) -> u64 {
        let b = self.bufs.borrow();
        match b.range(..=addr).next_back() {
            Some((&base, &(len, g))) if addr < base + len as u64 => g,
            _ => 0,
        }
    }

    /// Assert that whatever a transfer touches still belongs to the request that
    /// submitted it. This is what makes an abandoned transfer visible.
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

// --- the seams the runtime calls ---

/// Resolve a device path, creating it on first sight. The node id is carried in the path.
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
    devs.push(Device {
        node,
        fabric,
        blocks: BTreeMap::new(),
        len: 0,
        ops: 0,
    });
    s.paths.borrow_mut().insert(path.to_path_buf(), id);
    Ok(id)
}

/// Grow the simulated store to `want`, like `layout::size_if_needed`. Shrinking is
/// refused for the same reason it is there: layout offsets are absolute, so a smaller
/// store loses pages rather than moving them.
pub(crate) fn resize(dev: u32, want: u64) -> std::io::Result<()> {
    let s = shared();
    let mut devs = s.devs.borrow_mut();
    let d = &mut devs[dev as usize];
    if d.len > want {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            format!(
                "store is {} B, node.store.size_bytes is {want} B; a store cannot shrink",
                d.len
            ),
        ));
    }
    d.len = want;
    Ok(())
}

/// Simulated time, for the few places that need a clock rather than a timer.
pub(crate) fn now_us() -> u64 {
    shared().now.get()
}

/// The clock the whole system reads, virtual time expressed as a host `Instant` so that
/// nothing below this line needs to know it is being simulated. Falls back to the host
/// clock off a simulated thread, which is where the ordinary unit tests run.
pub(crate) fn clock() -> Instant {
    SHARED
        .with(|s| {
            s.borrow()
                .as_ref()
                .map(|s| s.base + Duration::from_micros(s.now.get()))
        })
        .unwrap_or_else(Instant::now)
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
    let mut pool = s.pool.borrow_mut();
    for (i, chunk) in src.chunks(BLOCK).enumerate() {
        devs[dev as usize].write(&mut pool, off / BLOCK as u64 + i as u64, chunk);
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
    let who = Who {
        node,
        core,
        idx,
        seq,
    };
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
    completion: (u32, u16),
) {
    let s = shared();
    let (node, core) = s.here.get();
    let (idx, seq) = completion;
    let who = Who {
        node,
        core,
        idx,
        seq,
    };
    s.live.borrow_mut().insert(who);
    s.held.borrow_mut().insert(who, s.generation(addr));
    let read = matches!(kind, Kind::Read);

    // The timeout is armed alongside: whichever event reaches the slab first wins, and
    // the other finds the op already gone.
    if let Some(t) = timeout {
        s.at(
            t.as_micros() as u64,
            What::Wake {
                who,
                res: -libc::ETIME,
            },
        );
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
            // Lost: only the timeout can finish this op now.
            s.hit(Hit::CutSubmit);
            return;
        }
        // A straggler is slow in everything it sends, not just the first piece it sends.
        let slow = if s.faults.borrow().slow(node, peer) {
            s.hit(Hit::Slow);
            SLOW_US
        } else {
            0
        };
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
            s.hit(Hit::Split);
            s.owed.borrow_mut().insert(who, (pieces, len as i32));
        }
        for p in 0..pieces {
            let at = p * mdts;
            let n = mdts.min(len - at);
            let delay = slow + if p == 0 { lat } else { s.latency() };
            if s.chance(s.faults.borrow().drop) {
                s.hit(Hit::Drop);
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
        s.at(
            lat,
            What::Disk {
                who,
                dev,
                read,
                off,
                addr,
                len,
            },
        );
    }
}

// --- the simulator ---

/// What a request slot is being used for, so a finished future can be routed.
enum Use {
    /// A client request, under the id `submit` returned.
    Client(u64),
    /// A frame from a peer, owed a reply. `wire` is the simulator's copy of the payload,
    /// and is what the handler runs against.
    Frame {
        origin: Who,
        read: bool,
        wire: Box<[u8]>,
        back: u64,
    },
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
    /// Frames a node of another zone has sent this one. Warming exists to remove
    /// cross-zone traffic, so a test measures this rather than a cache counter: it is the
    /// cost the client actually pays.
    crossings: u64,
    /// `WARM` frames this node has been sent, at either stage.
    warms: u64,
}

/// What the cluster still owes, so a test can wait for quiet rather than for a duration
/// it guessed. A run that has drained is the only place a claim about convergence, or
/// about a resource having been given back, means anything.
#[derive(Copy, Clone, Debug)]
pub struct Status {
    /// Client requests submitted and not yet answered.
    pub clients: usize,
    /// Operations the runtime is still carrying.
    pub ops: usize,
    /// Scheduled events other than the maintenance tick. A cluster left alone still
    /// sweeps its groups and still arms the next timer, so this counts what is on the
    /// calendar rather than what is outstanding, and quiet does not require it to be zero.
    pub events: usize,
    /// Commands with pieces still owed to them.
    pub split: usize,
}

impl Status {
    /// Nothing is in flight: no client is waiting, no operation is being carried, and no
    /// command is short of pieces. Anti-entropy never stops, so this is the gap between
    /// its sweeps rather than the end of all activity.
    pub fn idle(&self) -> bool {
        self.clients == 0 && self.ops == 0 && self.split == 0
    }
}

/// A client request's buffer, which must outlive the request, and the node serving it.
struct Pending {
    buf: Box<[u8]>,
    node: usize,
    /// Whether the request is still in flight. A completed request keeps its buffer so
    /// the caller can read what came back, but it is no longer the node's to fail: a
    /// crash must not rewrite a result someone has already been told.
    live: bool,
    /// Whether anything came back in the buffer. Only a read has bytes worth keeping
    /// once it is done; holding a finished write's page would mean a campaign's memory
    /// grew with the number of operations it had performed rather than with the number
    /// it had in flight, which for the 4 MiB class is a gigabyte a few hundred fills in.
    read: bool,
}

/// How a cluster is shaped. Groups, peers, extents and devices derive from these.
#[derive(Clone)]
pub struct Options {
    pub nodes: u32,
    pub cores: usize,
    pub seed: u64,
    /// 4 KiB pages in the small extent.
    pub pages: u64,
    /// 4 MiB pages in the immutable extent, or zero for none.
    pub huge_pages: u64,
    /// `cache_admit` for every extent the sim declares: 0 caches nothing, 1 caches on
    /// first sight, `n` caches once a page has been read `n` times in a decay interval.
    pub cache_admit: u32,
    /// The rate the backing device is willing to be driven at, or zero for unmetered.
    pub device_iops: u64,
    /// Whether every node opens every other. `false` keeps only the nodes it shares a
    /// group with, plus the gateways of every other zone, which is what makes hundreds of
    /// nodes affordable.
    pub clique: bool,
    /// How many zones the nodes are split into. `nodes` must divide evenly by this and
    /// leave at least three per zone, since a zone's catalog is built from its own
    /// members. Zone 1 owns every extent; the rest read across the fabric.
    pub zones: u32,
    /// Whether zone 1's immutable extent asks the other zones to be kept warm. Needs
    /// `zones > 1` and `huge_pages > 0` to mean anything, and `cache_admit` non-zero at
    /// the destination for a warm arrival to be admitted.
    pub warm: bool,
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
            cache_admit: 0,
            device_iops: 0,
            clique: true,
            zones: 1,
            warm: false,
            mdts: 0,
            faults: Faults::default(),
        }
    }
}

/// The one universe every simulated node shares. One is enough: the partitioning it
/// enforces is a property of which namespaces the control plane hands out, and there is
/// no control plane below this line.
pub const UNIVERSE: u32 = 1;

/// Device ids the simulator declares, each mapping the one extent of its class.
pub const SMALL: u64 = 1;
pub const HUGE: u64 = 2;

/// Where the small extent sits in the universe. The huge one follows it, rounded up to
/// the 4 MiB boundary a 4 MiB extent has to start on.
const SMALL_BASE: u64 = 0;

/// The first 4 MiB boundary past a small extent of `pages` pages.
fn huge_base(pages: u64) -> u64 {
    (SMALL_BASE + pages).next_multiple_of(crate::config::HUGE_BLOCKS)
}

pub struct Sim {
    s: Rc<Shared>,
    base: Instant,
    opts: Options,
    nodes: Vec<NodeState>,
    pending: BTreeMap<u64, Pending>,
    results: BTreeMap<u64, Result<(), i32>>,
    next_id: u64,
    done: Vec<(u32, Result<(), Errno>)>,
    /// Retired request buffers, by length. A finished request's memory goes straight back
    /// into service, the way ublk re-arms a tag onto the next request's pages, so
    /// anything still pointing at it sees the next request's bytes. Nothing is ever
    /// freed: an op that outlives its buffer is a bug, not a crash.
    spare: BTreeMap<usize, Vec<Box<[u8]>>>,
}

impl Sim {
    pub fn new(opts: Options) -> std::io::Result<Sim> {
        assert!(opts.nodes >= 3, "consensus needs three nodes");
        assert!(opts.zones >= 1, "there is always at least one zone");
        assert!(
            opts.nodes.is_multiple_of(opts.zones),
            "zones are homogeneous, so the nodes must divide evenly between them"
        );
        assert!(
            opts.nodes / opts.zones >= 3,
            "a zone builds its catalog from its own members, so it needs three"
        );
        assert!(
            (opts.mdts as usize).is_multiple_of(BLOCK),
            "mdts must be a whole number of blocks"
        );
        raise_files();
        let base = Instant::now();
        let s = Rc::new(Shared {
            now: Cell::new(0),
            base,
            here: Cell::new((0, 0)),
            devs: RefCell::new(Vec::new()),
            pool: RefCell::new(Pool::default()),
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
            hits: Cell::new([0; Hit::ALL.len()]),
        });
        SHARED.with(|c| *c.borrow_mut() = Some(s.clone()));
        s.at(TICK_US, What::Tick);

        let mut sim = Sim {
            s,
            base,
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
            // Nodes of a zone are homogeneous, so every one plans the same device. Across
            // zones they legitimately differ: a zone that owns no extent plans nothing
            // and reads everything.
            if let Some(first) = sim.nodes.iter().find(|n| n.cfg.node.zone == cfg.node.zone) {
                assert_eq!(
                    (cfg.small_pages(), cfg.huge_pages()),
                    (first.cfg.small_pages(), first.cfg.huge_pages()),
                    "node {} plans a different store to node {}",
                    i + 1,
                    first.id
                );
            }

            crate::layout::size_if_needed(&cfg.node.store, &cfg)?;
            crate::layout::format(&cfg.node.store, &cfg)?;
            sim.nodes.push(NodeState {
                id: i + 1,
                cfg,
                workers: None,
                node: None,
                dp: std::ptr::null(),
                free: Vec::new(),
                slots: Vec::new(),
                crossings: 0,
                warms: 0,
            });
            sim.boot(i as usize)?;
        }
        Ok(sim)
    }

    // --- topology ---

    /// Nodes per zone. Ids are handed out in contiguous blocks, so zone `z` holds
    /// `(z - 1) * per .. z * per`, one-based.
    fn per_zone(&self) -> u32 {
        self.opts.nodes / self.opts.zones
    }

    fn zone_of(&self, id: u32) -> u32 {
        (id - 1) / self.per_zone() + 1
    }

    /// The nodes of `zone`, in id order.
    fn zone_nodes(&self, zone: u32) -> Vec<u32> {
        let per = self.per_zone();
        ((zone - 1) * per + 1..=zone * per).collect()
    }

    /// Consensus groups of one zone: every window of three consecutive members, so each
    /// sits in three groups and no group repeats a member. One group per member, which
    /// keeps the catalog balanced: `3 * k` seats spread three apiece.
    ///
    /// Every column of this catalog holds every node of the zone, which is why the
    /// simulator gives every node cohort zero: the cohort is the catalog column, and here
    /// all three columns are the same set. A warm push therefore places one copy per zone
    /// rather than three, and every reader in that zone agrees on which node holds it.
    /// Three genuinely disjoint cohorts are covered by the config unit tests.
    fn group(&self, zone: u32, g: u32) -> [u32; 3] {
        let m = self.zone_nodes(zone);
        let k = m.len() as u32;
        [
            m[(g % k) as usize],
            m[((g + 1) % k) as usize],
            m[((g + 2) % k) as usize],
        ]
    }

    /// The nodes of `zone` that answer traffic from outside it. Three is enough to
    /// exercise the ring's fall-through without making the peer set quadratic.
    fn gateways_of(&self, zone: u32) -> Vec<u32> {
        let mut m = self.zone_nodes(zone);
        m.truncate(3);
        m
    }

    fn peers_of(&self, id: u32) -> Vec<u32> {
        let n = self.opts.nodes;
        if self.opts.clique {
            return (1..=n).filter(|&p| p != id).collect();
        }
        // Only the nodes this one shares a group with, plus the way out of its own zone:
        // a clique is O(n^2) links, and every link is a registered device.
        let zone = self.zone_of(id);
        let mut out = BTreeSet::new();
        for g in 0..self.per_zone() {
            let m = self.group(zone, g);
            if m.contains(&id) {
                out.extend(m.iter().copied().filter(|&p| p != id));
            }
        }
        for z in 1..=self.opts.zones {
            if z != zone {
                out.extend(self.gateways_of(z));
            }
        }
        out.into_iter().collect()
    }

    fn config_text(&self, id: u32) -> String {
        let o = &self.opts;
        let mut t = String::new();
        t.push_str("generation 1\n");
        let zone = self.zone_of(id);
        t.push_str(&format!(
            "node id={id} zone={zone} cohort=0 store=/sim/n{id}/store size={} max_iops={}\n",
            store_bytes(o),
            o.device_iops
        ));
        // The index ceiling is a real check; give it room for the extents we declare.
        let idx = o.pages * crate::alloc::INDEX_BYTES_PER_PAGE + (1 << 20);
        t.push_str(&format!(
            "policy max_index_bytes={idx} occ_bytes={} cache_index_bytes={CACHE_INDEX_BYTES}\n",
            1 << 20
        ));
        // The fabric namespace exports as a ublk minor of its own, which has to differ from
        // every device minor this node asks for. The devices are 1 and 2.
        t.push_str(&format!("universe {UNIVERSE} epoch=1 fabric_device_id=9\n"));
        for p in self.peers_of(id) {
            t.push_str(&format!("peer id={p} device=/sim/n{p}/fabric\n"));
        }
        // A catalog describes this node's own zone and no other.
        for g in 0..self.per_zone() {
            let m = self.group(zone, g);
            t.push_str(&format!("group {} {} {}\n", m[0], m[1], m[2]));
        }
        for z in 1..=o.zones {
            if z != zone {
                let gw: Vec<String> = self.gateways_of(z).iter().map(u32::to_string).collect();
                t.push_str(&format!("zone id={z} gateways={}\n", gw.join(",")));
            }
        }
        // Every extent is homed in zone 1. The other zones map the same devices, so a
        // read there is a cross-zone read of the same page.
        t.push_str(&format!(
            "extent id=1 base={SMALL_BASE} pages={} kind=lww zone=1 cache_admit={}\n",
            o.pages, o.cache_admit
        ));
        t.push_str(&format!("device {SMALL} extents=1\n"));
        if o.huge_pages > 0 {
            let warm: String = match o.warm {
                true if o.zones > 1 => {
                    let z: Vec<String> = (2..=o.zones).map(|z| z.to_string()).collect();
                    format!(" warm_zones={}", z.join(","))
                }
                _ => String::new(),
            };
            t.push_str(&format!(
                "extent id=2 base={} pages={} kind=immutable_4m zone=1 cache_admit={}{warm}\n",
                huge_base(o.pages),
                o.huge_pages,
                o.cache_admit
            ));
            t.push_str(&format!("device {HUGE} extents=2\n"));
        }
        t
    }

    // --- lifecycle ---

    /// Bring a node up against whatever its device already holds. Restart is the same
    /// call, which is the whole of the recovery test.
    fn boot(&mut self, i: usize) -> std::io::Result<()> {
        let cores = self.opts.cores;
        let cfg = self.nodes[i].cfg.clone();
        let workers = SimNode::<server::Server>::new(cores, &SERVER, self.base)?;
        let node = Box::new(server::Node::new());
        let cfgr = crate::runtime::Configurator::sim(cores);
        // `attach` runs on the control thread in production and touches no worker state,
        // so it needs no `Local`.
        let dp: *const () = Box::leak(Box::new(node.attach(&cfgr, cfg)?)) as *const _ as *const ();
        let rows = cfgr.take_core_state::<server::CoreState>();
        let n = &mut self.nodes[i];
        for (c, row) in rows.into_iter().enumerate() {
            workers.at(c).publish(1, dp);
            workers
                .at(c)
                .install_core_state(Box::leak(Box::new(row)) as *const _ as *const ());
        }
        n.free = (0..cores)
            .map(|_| (0..slots_per_worker()).collect())
            .collect();
        n.slots = (0..cores)
            .map(|_| (0..slots_per_worker()).map(|_| None).collect())
            .collect();
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
        // In-flight frame buffers go back into service rather than away: a scheduled
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
        // Its in-flight client requests can never complete, so fail them now. A request
        // that already finished keeps both its result and its payload: the caller may
        // have been told, and a crash cannot un-tell them.
        let lost: Vec<u64> = self
            .pending
            .iter()
            .filter(|(_, p)| p.node == i && p.live)
            .map(|(&k, _)| k)
            .collect();
        for k in lost {
            if let Some(p) = self.pending.remove(&k) {
                self.put_buf(p.buf);
            }
            self.results.insert(k, Err(libc::EIO));
        }
        self.s.hit(Hit::Crash);
    }

    pub fn restart(&mut self, i: usize) -> std::io::Result<()> {
        assert!(self.nodes[i].workers.is_none(), "node is already up");
        self.s.hit(Hit::Restart);
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
    /// budget meters, so a test can check the budget was held to.
    pub fn device_ops(&self, node: usize) -> u64 {
        let path = self.nodes[node].cfg.node.store.as_path();
        let dev = *self.s.paths.borrow().get(path).expect("store device");
        self.s.devs.borrow()[dev as usize].ops
    }

    /// Frames this node has been sent from a node in another zone since boot.
    ///
    /// Warming aims to stop a reader in a consuming zone crossing to the home zone, so
    /// the honest measure is how much crossing there is. Counted at delivery.
    pub fn crossings(&self, node: usize) -> u64 {
        self.nodes[node].crossings
    }

    /// `WARM` frames this node has been sent since boot, counting both the one a writing
    /// zone sends its gateway and the one that gateway relays to a holder.
    pub fn warms(&self, node: usize) -> u64 {
        self.nodes[node].warms
    }

    /// How many times the cluster has reached a given path since the simulation began.
    ///
    /// A campaign asserts against these: an invariant nothing reaches passes for the
    /// wrong reason, so the paths a run took are part of what it proved.
    pub fn hits(&self, h: Hit) -> u64 {
        self.s.hits.get()[h as usize]
    }

    /// What is still owed to whoever asked for it.
    pub fn status(&self) -> Status {
        let evs = self.s.evs.borrow();
        Status {
            clients: self.pending.values().filter(|p| p.live).count(),
            ops: self.s.live.borrow().len(),
            // The maintenance tick never stops, so it is not work; everything else is.
            events: evs
                .iter()
                .filter(|Reverse(e)| !matches!(e.what, What::Tick))
                .count(),
            split: self.s.owed.borrow().len(),
        }
    }

    /// Check every up node's internal state. The cheapest place to catch a bug is the
    /// action that caused it, not the read that eventually noticed.
    pub fn check_invariants(&self) -> Result<(), String> {
        for (i, n) in self.nodes.iter().enumerate() {
            let Some(w) = n.workers.as_ref() else {
                continue;
            };
            // Asked of each worker in turn, because each owns its own share: there is no
            // one place left where a node's state can be read whole.
            for c in 0..w.cores() {
                w.at(c)
                    .core_state()
                    .invariants()
                    .map_err(|e| format!("node {i}, core {c}: {e}"))?;
            }
        }
        Ok(())
    }

    /// Pages part-way through arriving, across every up node.
    pub fn assemblies(&self) -> usize {
        self.nodes
            .iter()
            .filter_map(|n| n.workers.as_ref())
            .map(|w| {
                (0..w.cores())
                    .map(|c| w.at(c).core_state().assemblies())
                    .sum::<usize>()
            })
            .sum()
    }

    /// The node indices holding a small page's consensus group. What makes a write to
    /// any other node a non-member's, which is a different path through the server.
    pub fn small_members(&self, lba: u64) -> Vec<usize> {
        self.members(SMALL_BASE + lba)
    }

    /// As [`Sim::small_members`], for a huge page.
    pub fn huge_members(&self, page: u64) -> Vec<usize> {
        self.members(huge_base(self.opts.pages) + page * crate::config::HUGE_BLOCKS)
    }

    fn members(&self, addr: u64) -> Vec<usize> {
        let addr = crate::alloc::GlobalAddr::new(UNIVERSE, addr);
        let cfg = &self.nodes[0].cfg;
        let group = cfg.group(addr.0);
        let Some(u) = cfg.universe(group.universe()) else {
            return Vec::new();
        };
        u.catalog[group.index() as usize]
            .iter()
            .filter_map(|m| self.nodes.iter().position(|n| n.id == *m))
            .collect()
    }

    /// Damage the persisted bytes of one replica of a small page. `replica` is a
    /// position in the page's consensus group; the return value is that node's index.
    pub fn corrupt_small_replica(&mut self, lba: u64, replica: usize) -> usize {
        let (node, geo, slot, _) = self
            .small_replica_location(lba, replica)
            .unwrap_or_else(|| panic!("small page {lba} is not live on replica {replica}"));
        let path = self.nodes[node].cfg.node.store.as_path();
        let dev = *self.s.paths.borrow().get(path).expect("store device");
        let off = geo.slot_off(Class::Small, slot);
        let mut devs = self.s.devs.borrow_mut();
        let d = &mut devs[dev as usize];
        let mut block = [0u8; BLOCK];
        d.read(off / BLOCK as u64, &mut block);
        block[17] ^= 0xff;
        d.write(&mut self.s.pool.borrow_mut(), off / BLOCK as u64, &block);
        node
    }

    /// Whether a replica is holding a small page at all, which is what makes it
    /// something a campaign can damage and expect to see repaired.
    pub fn small_replica_live(&self, lba: u64, replica: usize) -> bool {
        self.small_replica_location(lba, replica).is_some()
    }

    /// Whether a replica's persisted page still matches its mblock entry's `data_crc`.
    pub fn small_replica_valid(&self, lba: u64, replica: usize) -> bool {
        let Some((node, geo, slot, entry)) = self.small_replica_location(lba, replica) else {
            return false;
        };
        let path = self.nodes[node].cfg.node.store.as_path();
        let Some(&dev) = self.s.paths.borrow().get(path) else {
            return false;
        };
        let mut page = [0u8; BLOCK];
        self.s.devs.borrow()[dev as usize]
            .read(geo.slot_off(Class::Small, slot) / BLOCK as u64, &mut page);
        layout::page_crc(entry.addr, entry.version, &page) == entry.data_crc
    }

    fn small_replica_location(
        &self,
        lba: u64,
        replica: usize,
    ) -> Option<(usize, Geometry, u32, Entry)> {
        let addr = crate::alloc::GlobalAddr::new(UNIVERSE, SMALL_BASE + lba);
        let cfg = &self.nodes.first()?.cfg;
        let group = cfg.group(addr.0);
        let u = cfg.universe(group.universe())?;
        let member = *u.catalog.get(group.index() as usize)?.get(replica)?;
        let node = self.nodes.iter().position(|n| n.id == member)?;
        let path = self.nodes[node].cfg.node.store.as_path();
        let geo = layout::read_geometry(path).ok()?;
        let dev = *self.s.paths.borrow().get(path)?;
        let mut raw = [0u8; BLOCK];

        for id in 0..geo.mblocks(Class::Small) as u32 {
            let mut best: Option<(u64, [u8; BLOCK])> = None;
            for copy in 0..2u8 {
                raw_read(dev, geo.mblock_off(Class::Small, id, copy), &mut raw).ok()?;
                let Some(h) = layout::get_header(&raw) else {
                    continue;
                };
                if h.class != Class::Small || h.mblock_id != id {
                    continue;
                }
                if best
                    .as_ref()
                    .is_none_or(|(generation, _)| h.generation > *generation)
                {
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

    // --- client IO ---

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
        // Handing it out invalidates whatever still points at it.
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
    fn submit(&mut self, i: usize, dev: u64, op: Op, lba: u64, data: Option<&[u8]>) -> u64 {
        let id = self.next_id;
        self.next_id += 1;
        let huge = dev == HUGE;
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
        self.pending.insert(
            id,
            Pending {
                buf,
                node: i,
                live: true,
                read: op == Op::Read,
            },
        );
        self.start(
            i,
            core,
            Use::Client(id),
            Request {
                dev,
                op,
                lba,
                buf: b,
            },
        );
        id
    }

    pub fn write(&mut self, i: usize, lba: u64, fill: u8) -> u64 {
        let page = vec![fill; BLOCK];
        self.submit(i, SMALL, Op::Write, lba, Some(&page))
    }

    /// Write a page of arbitrary bytes rather than one repeated byte. A single fill has
    /// only 256 values, so a long run reuses them and a read can no longer say which
    /// write it came from; a pattern names its write.
    pub fn write_with(&mut self, i: usize, lba: u64, page: &[u8]) -> u64 {
        assert_eq!(page.len(), BLOCK, "a small write is one page");
        self.submit(i, SMALL, Op::Write, lba, Some(page))
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

    /// As [`Sim::write_with`], for the immutable class.
    pub fn write_huge_with(&mut self, i: usize, page: u64, buf: &[u8]) -> u64 {
        assert_eq!(buf.len(), 4 << 20, "a huge write is one whole page");
        self.submit(i, HUGE, Op::Write, page * 1024, Some(buf))
    }

    pub fn read_huge(&mut self, i: usize, page: u64) -> u64 {
        self.submit(i, HUGE, Op::Read, page * 1024, None)
    }

    pub fn result(&self, id: u64) -> Option<Result<(), i32>> {
        self.results.get(&id).copied()
    }

    /// The bytes a finished read returned. `None` once its node has crashed under it,
    /// or once the caller has done with it.
    pub fn payload(&self, id: u64) -> Option<&[u8]> {
        self.pending.get(&id).map(|p| &p.buf[..])
    }

    /// Give a finished read's bytes back. The result stays: a caller may still ask what
    /// it was told long after it has stopped caring what came back. A long campaign has
    /// to say this, or it holds every page it ever read at once.
    pub fn forget(&mut self, id: u64) {
        if let Some(p) = self.pending.get(&id)
            && !p.live
            && let Some(p) = self.pending.remove(&id)
        {
            self.put_buf(p.buf);
        }
    }

    /// A live worker on `i`, chosen by address so a page always lands on the same one.
    fn pick(&self, i: usize, lba: u64) -> Option<usize> {
        let n = self.nodes.get(i)?;
        let w = n.workers.as_ref()?;
        Some((lba as usize) % w.cores())
    }

    /// Hand a request to a worker, recording what its slot is owed to. Returns false if
    /// the worker has no slot free.
    fn start(&mut self, i: usize, core: usize, u: Use, req: Request) -> bool {
        let Some(slot) = self.nodes[i].free[core].pop_front() else {
            match u {
                Use::Client(id) => {
                    self.results.insert(id, Err(libc::EAGAIN));
                    // Refused before it ever started, so it is not in flight and never
                    // will be. Leaving it live would mean the cluster could never be
                    // called quiet again, since nothing later completes a request that
                    // no worker took.
                    if let Some(p) = self.pending.remove(&id) {
                        self.put_buf(p.buf);
                    }
                }
                Use::Frame {
                    origin, back, wire, ..
                } => {
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
                // back into service. What a read returned is kept as a copy for
                // `payload`; a write's page is not, since nothing came back in it.
                if let Some(p) = self.pending.get_mut(&id) {
                    p.live = false;

                    if p.read {
                        let copy = p.buf.to_vec().into_boxed_slice();
                        let old = std::mem::replace(&mut p.buf, copy);
                        self.put_buf(old);
                    } else if let Some(p) = self.pending.remove(&id) {
                        self.put_buf(p.buf);
                    }
                }
            }
            Some(Use::Frame {
                origin,
                read,
                wire,
                back,
            }) => {
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
            self.s.hit(Hit::CutReply);
            return;
        }
        // A straggler is slow on the way back too, which is what makes a reply arrive
        // after the requester has given up and reused the memory it named.
        let slow = if self.s.faults.borrow().slow(from, origin.node) {
            self.s.hit(Hit::Slow);
            SLOW_US
        } else {
            0
        };
        let lat = slow + self.s.latency();
        self.s.at(
            lat,
            What::Reply {
                from,
                who: origin,
                res,
                back,
                wire,
            },
        );
    }

    // --- the loop ---

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

    /// Claim an op, so it is completed exactly once. A late timeout, or a reply that lost
    /// a race with one, finds it gone.
    fn take(&self, who: Who) -> bool {
        let took = self.s.live.borrow_mut().remove(&who);
        if took {
            self.s.owed.borrow_mut().remove(&who);
            self.s.held.borrow_mut().remove(&who);
        }
        took
    }

    fn complete(&mut self, who: Who, res: i32) {
        let Some(i) = self.nodes.iter().position(|n| n.id == who.node) else {
            return;
        };
        let Some(w) = self.nodes[i].workers.as_mut() else {
            return;
        };
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
            What::Reply {
                from,
                who,
                res,
                back,
                wire,
            } => {
                if !self.s.live.borrow().contains(&who) {
                    return;
                }
                // A partition that opened while this was in flight swallows it here. A
                // cut that only refused new sends would let a reply cross a link that is
                // supposed to be down.
                if self.s.faults.borrow().cut(from, who.node) {
                    self.s.hit(Hit::CutReply);
                    return;
                }
                // SAFETY: the op was still live, so the buffer it named is alive.
                if let Some(w) = wire {
                    self.s.still_ours(&who, back, "a reply");
                    let dst = unsafe { std::slice::from_raw_parts_mut(back as *mut u8, w.len()) };
                    dst.copy_from_slice(&w);
                }
                // A split transfer still has one completion: the last piece home ends it,
                // and any failed piece ends it early with that failure.
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
            What::Disk {
                who,
                dev,
                read,
                off,
                addr,
                len,
            } => {
                if !self.s.live.borrow().contains(&who) {
                    return;
                }
                self.s.still_ours(&who, addr, "a storage transfer");
                self.take(who);
                let res = self.transfer(dev, read, off, addr, len);
                self.complete(who, res);
            }
            What::Send {
                from,
                to,
                read,
                lba,
                back,
                len,
                tries,
            } => self.deliver(from, to, read, lba, back, len, tries),
        }
    }

    /// Perform a storage transfer, injecting failure where the device would report it.
    fn transfer(&self, dev: u32, read: bool, off: u64, addr: u64, len: u32) -> i32 {
        let (bad, rot) = {
            let f = self.s.faults.borrow();
            (f.io_error, f.corrupt)
        };
        if self.s.chance(bad) {
            self.s.hit(Hit::IoError);
            return -libc::EIO;
        }
        let n = len as usize;
        // SAFETY: the op is live, so `addr` names a buffer of ours that is still alive.
        let mem = unsafe { std::slice::from_raw_parts_mut(addr as *mut u8, n) };
        let base = off / BLOCK as u64;
        let mut devs = self.s.devs.borrow_mut();
        let mut pool = self.s.pool.borrow_mut();
        let d = &mut devs[dev as usize];
        for (i, chunk) in mem.chunks_mut(BLOCK).enumerate() {
            if read {
                d.read(base + i as u64, chunk);
            } else {
                d.write(&mut pool, base + i as u64, chunk);
            }
        }
        if read && self.s.chance(rot) {
            self.s.hit(Hit::Corrupt);
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
    fn deliver(
        &mut self,
        from: Who,
        to: u32,
        read: bool,
        lba: u64,
        back: u64,
        len: u32,
        tries: u32,
    ) {
        // The command was cancelled, by its link timeout or because its node went away.
        // A cancelled command's buffer is the initiator's again, so nothing may be read
        // out of it.
        if !self.s.live.borrow().contains(&from) {
            return;
        }
        // The link went down while this was in flight. Judging a cut only at submission
        // would let a frame cross a partition that opened behind it.
        if self.s.faults.borrow().cut(from.node, to) {
            self.s.hit(Hit::CutDeliver);
            return;
        }
        let Some(i) = self.nodes.iter().position(|n| n.id == to) else {
            return;
        };
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
            self.s.hit(Hit::Saturated);
            if tries < 64 {
                let e = What::Send {
                    from,
                    to,
                    read,
                    lba,
                    back,
                    len,
                    tries: tries + 1,
                };
                self.s.at(LATENCY_US, e);
            }
            return;
        }
        // Counted here rather than at submission: this is the point the frame is
        // certainly being served, so a delivery retried for want of a slot counts once.
        self.s.hit(Hit::Piece);
        if self.zone_of(from.node) != self.zone_of(to) {
            self.s.hit(Hit::Crossing);
            self.nodes[i].crossings += 1;
        }
        if let Ok(cmd) = crate::fabric::Cmd::decode(lba, len as usize, read)
            && matches!(cmd, crate::fabric::Cmd::Warm { .. })
        {
            self.s.hit(Hit::Warm);
            self.nodes[i].warms += 1;
        }
        let op = if read { Op::Read } else { Op::Write };
        // The wire is read out of the sender's buffer now, at the instant the transport
        // would have.
        self.s.still_ours(&from, back, "a frame");
        let mut wire = self.take_buf(len as usize);
        // SAFETY: the op is live, so `back` names a buffer of ours that is still alive.
        wire.copy_from_slice(unsafe {
            std::slice::from_raw_parts(back as *const u8, len as usize)
        });
        let b = sim_buf(0, wire.as_mut_ptr() as u64, len);
        let u = Use::Frame {
            origin: from,
            read,
            wire,
            back,
        };
        self.start(
            i,
            core,
            u,
            Request {
                // A frame arrives on the universe's own fabric device, the only thing on
                // the wire that names the universe.
                dev: server::fabric_key(UNIVERSE),
                op,
                lba,
                buf: b,
            },
        );
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

/// One request slot per (device, tag) the runtime would have armed. The simulator hands
/// them out itself, so this is only a ceiling.
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
