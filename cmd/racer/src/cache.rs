//! Cooperative caching: which nodes keep a copy of which page, so reads of a hot page do not
//! all land on the three that own it.
//!
//! No consistency of its own: every cached read is confirmed by the mandatory metadata round,
//! so staleness is self-correcting and there is no invalidation protocol. Never a correctness
//! dependency; every entry point may decline, costing one ordinary read.
//!
//!   * **No directory.** Placement is a rendezvous hash over the reader's own cohort, so
//!     any reader computes the replica set locally.
//!   * **No per-key state.** Hotness is a count-min sketch, width is arithmetic on it, the
//!     width hint is a direct-mapped byte array, and the victim contest reads that same
//!     sketch. Nothing grows with the key space; nothing allocates after `open`.
//!   * **No durability.** The map is volatile, writes are in place and `Buffered`, and the
//!     region is carved at format time. A torn cache page is unreachable after a restart
//!     because nothing points at it, so the allocator's out-of-place rule does not apply.
//!
//! The rendezvous hash is [`config::mix`]; never on the wire, so only local computability and
//! `R(k,w)` being a prefix of `R(k,w+1)` matter. A shedding replica answers `MISSING` rather
//! than its own busy status, since only four errnos survive nvmet and the reader's fallback is
//! identical either way. Admission is per extent: `Extent::cache_admit` is the demand a page
//! must show to enter, in requests per decay interval, so it is bounded by the sketch ceiling
//! of 15; it reserves no capacity and ranks no extents. Priority is global: a candidate must
//! out-rank the entry at the clock hand on measured demand alone, which lets an extent admit
//! everything without its scan sweeping the cache away.

use std::cell::RefCell;
use std::collections::HashMap;
use std::time::{Duration, Instant};

use crate::alloc::{Allocator, GlobalAddr, Pressure};
use crate::config::{self, Config, rank};
use crate::layout::{self, Class};
use crate::paxos::Register;
use crate::runtime::{self, Buf, CoreId, Disk, Durability};
use crate::server::{self, Server};

// --- tunables ---
//
// Not configuration. Sketch geometry, decay interval and width cap are correctness-
// independent, so a node picks them without the control plane knowing anything.

/// Count-min rows. Four is where the collision rate stops mattering next to the 4-bit
/// saturation.
const ROWS: usize = 4;
/// Counters per row when this core holds no slots, and the floor everywhere. A power of two so
/// the index is a mask; as packed nibbles that is 128 KiB, which is L2-resident.
#[cfg(not(feature = "sim"))]
const COLS_MIN: usize = 1 << 16;
#[cfg(feature = "sim")]
const COLS_MIN: usize = 1 << 10;
/// Counters per row at the ceiling. The sketch must scale with the cache, or a store whose
/// tail holds millions of slots filters admissions off a sketch sized for tens of thousands
/// and every cold page looks hot. The cap keeps it L3-resident.
#[cfg(not(feature = "sim"))]
const COLS_MAX: usize = 1 << 21;
#[cfg(feature = "sim")]
const COLS_MAX: usize = 1 << 12;
/// A nibble saturates here.
const COUNTER_MAX: u8 = 15;
/// Halvings that take a saturated counter to zero: `15 -> 7 -> 3 -> 1 -> 0`.
const ZEROED_AFTER: u32 = COUNTER_MAX.ilog2() + 1;
/// Halving at this interval turns the sketch into an exponentially weighted rate.
const DECAY: Duration = Duration::from_millis(250);
/// `W_max` before the cohort size caps it.
const W_CAP: u8 = 64;
/// Reader-side width hints, direct mapped, sized like the sketch and for the same reason. A
/// collision mis-hints one address, costing one `MISSING` and a fallback, so there is nothing
/// to resolve.
#[cfg(not(feature = "sim"))]
const HINTS_MIN: usize = 1 << 16;
#[cfg(feature = "sim")]
const HINTS_MIN: usize = 1 << 10;
#[cfg(not(feature = "sim"))]
const HINTS_MAX: usize = 1 << 22;
#[cfg(feature = "sim")]
const HINTS_MAX: usize = 1 << 13;

/// DRAM a resident cache slot costs: the [`Slot`] record plus its share of the map.
/// `policy.cache_index_bytes` divided by this bounds slots per node, which stops a large tail
/// from turning into an OOM.
pub const BYTES_PER_SLOT: u64 = 48;

/// How often core 0 reconsiders the split between the classes.
const REBALANCE: Duration = Duration::from_secs(1);
/// How far ahead one class must be, in confirmed hits per byte held, before a chunk moves to
/// it. A move costs whatever the chunk held, so this follows a workload change and not one
/// interval's noise.
const REBALANCE_MARGIN: u64 = 2;
/// The share of `policy.cache_index_bytes` the cache builds before serving a read. The policy
/// is a ceiling, not an allocation, and paying it up front costs DRAM and startup latency: at
/// the default that is twenty-odd million slot records to zero before the first request. The
/// rebalance grows the cache out of the pool instead.
const OPEN_SHARE: u64 = 64;
/// The most a class may grow out of the pool in one interval, as a fraction of what it already
/// holds. Proportional so growth is geometric, since drawing on the pool costs the other class
/// nothing; it stops the moment the class stops evicting.
const GROWTH_SHARE: u64 = 4;

/// Per-row salt, so the rows are independent hashes of the same address.
const SALT: [u64; ROWS] = [
    0x9e37_79b9_7f4a_7c15,
    0xc2b2_ae3d_27d4_eb4f,
    0x1656_67b1_9e37_79f9,
    0xff51_afd7_ed55_8ccd,
];

/// A power of two in `[lo, hi]` covering `want` entries. Both bounds are powers of two, so the
/// result indexes with a mask.
fn table_len(want: u64, lo: usize, hi: usize) -> usize {
    want.checked_next_power_of_two()
        .unwrap_or(hi as u64)
        .clamp(lo as u64, hi as u64) as usize
}

// --- sketch ---

/// Count-min with conservative update and periodic halving. Conservative update makes the
/// estimator one-sided: it may over-count but never under-count, and over-counting only
/// over-replicates a page, which `W_max` bounds and the next decay corrects.
struct Sketch {
    /// One packed nibble array per row.
    rows: [Box<[u8]>; ROWS],
    /// `cols - 1`, the index mask. Held rather than a const because the sketch is sized from
    /// the slots this core ended up with.
    mask: usize,
    /// Halvings since a counter was last made non-zero. At `ZEROED_AFTER` every counter is
    /// zero and decay has nothing left to do, which keeps an idle core, or a node whose cache
    /// is off, from walking megabytes of zeroes four times a second forever.
    since: u32,
}

impl Sketch {
    fn new(cols: usize) -> Sketch {
        debug_assert!(cols.is_power_of_two());
        Sketch {
            rows: std::array::from_fn(|_| vec![0u8; cols / 2].into_boxed_slice()),
            mask: cols - 1,
            // Born zero, so it is already as decayed as it can get.
            since: ZEROED_AFTER,
        }
    }

    fn cell(&self, row: usize, addr: u64) -> usize {
        config::mix(addr ^ SALT[row]) as usize & self.mask
    }

    fn get(&self, row: usize, i: usize) -> u8 {
        (self.rows[row][i >> 1] >> ((i & 1) * 4)) & 0xf
    }

    fn set(&mut self, row: usize, i: usize, v: u8) {
        let b = &mut self.rows[row][i >> 1];
        let sh = (i & 1) * 4;
        *b = (*b & !(0xf << sh)) | (v << sh);
        self.since = 0;
    }

    /// The rate estimate `q(k)`, in counts since the last halving.
    fn estimate(&self, addr: u64) -> u8 {
        (0..ROWS)
            .map(|r| self.get(r, self.cell(r, addr)))
            .min()
            .unwrap_or(0)
    }

    /// Record one request and return the new estimate. Only rows sitting at the current
    /// minimum are raised, which is the whole of "conservative".
    fn observe(&mut self, addr: u64) -> u8 {
        let cells: [usize; ROWS] = std::array::from_fn(|r| self.cell(r, addr));
        let min = (0..ROWS).map(|r| self.get(r, cells[r])).min().unwrap_or(0);
        if min == COUNTER_MAX {
            return min;
        }
        for (r, &c) in cells.iter().enumerate() {
            if self.get(r, c) == min {
                self.set(r, c, min + 1);
            }
        }
        min + 1
    }

    /// Halve every counter `n` times.
    ///
    /// One shift and one mask: the only bits a wider shift gets wrong are the `n` falling out
    /// of the high nibble into the low one, and `(0x0F >> n) * 0x11` clears exactly those. At
    /// `ZEROED_AFTER` the mask is zero, so a saturating step needs no special case. Runs a
    /// word at a time; the replicated mask cleans up the same.
    fn halve(&mut self, n: u32) {
        // Zeroes halve to zeroes. Skipping this keeps a node whose cache is off from walking
        // its whole sketch four times a second for the life of the process.
        if n == 0 || self.since >= ZEROED_AFTER {
            return;
        }
        self.since = self.since.saturating_add(n);
        let n = n.min(ZEROED_AFTER);
        let m = (0x0f_u8 >> n) * 0x11;
        let w = u64::from(m) * 0x0101_0101_0101_0101;
        for row in &mut self.rows {
            let mut words = row.chunks_exact_mut(8);
            for c in &mut words {
                let v = (u64::from_le_bytes(c.try_into().unwrap()) >> n) & w;
                c.copy_from_slice(&v.to_le_bytes());
            }
            for b in words.into_remainder() {
                *b = (*b >> n) & m;
            }
        }
    }
}

// --- placement ---

/// This node's cohort, as the config implies it: one roster per universe.
///
/// There is no cohort roster in the schema. `Node.cohort` names a catalog column, so a cohort
/// is the projection of a universe's catalog onto that column, and every node claiming column
/// `c` derives the same roster, which local computation of `R` requires. The schema does not
/// check that a node occupies its column in every group (a catalog may permute members to
/// spread the paxos member index), so the cohort is the control plane's label, not one derived
/// here. Per universe, because a replica is reached over the universe's own namespace and a
/// node elsewhere has no address for the page; the column index is shared, since a node holds
/// one cohort label everywhere.
#[derive(Default)]
pub(crate) struct Roster {
    me: u32,
    /// Sorted by universe id.
    cohorts: Box<[(u32, Box<[u32]>)]>,
    /// Whether any extent asks to be cached at all. A config where nobody opts in turns the
    /// whole cache off, and knowing that before the address is looked at saves a hop to
    /// another core to search an empty store.
    admits: bool,
}

impl Roster {
    pub(crate) fn of(cfg: &Config) -> Roster {
        let c = cfg.node.cohort as usize;
        let cohorts = cfg
            .universes()
            .iter()
            .map(|u| {
                let mut nodes: Vec<u32> = u.catalog.iter().map(|g| g[c]).collect();
                nodes.push(cfg.node.id);
                nodes.sort_unstable();
                nodes.dedup();
                (u.id, nodes.into_boxed_slice())
            })
            .collect::<Vec<_>>();
        Roster {
            me: cfg.node.id,
            cohorts: cohorts.into_boxed_slice(),
            admits: cfg
                .universes()
                .iter()
                .any(|u| u.extents.iter().any(|e| e.cache_admit > 0)),
        }
    }

    /// The cohort in one universe. Empty for a universe we hold no catalog for, the same
    /// answer as caching being off there.
    fn cohort(&self, universe: u32) -> &[u32] {
        match self.cohorts.binary_search_by_key(&universe, |(id, _)| *id) {
            Ok(i) => &self.cohorts[i].1,
            Err(_) => &[],
        }
    }

    /// The widest cohort we are in. `W_max` is a ceiling taken before the address is known, so
    /// the largest cohort is the right one to take it from.
    fn widest(&self) -> usize {
        self.cohorts.iter().map(|(_, n)| n.len()).max().unwrap_or(0)
    }
}

// --- store ---

/// One cache slot. Subtractive next to an allocator entry: no CRC, no generation, no A/B copy,
/// no group commit. The register is here only so a hit can be confirmed against the quorum;
/// nothing here survives a restart.
#[derive(Clone, Copy, Default)]
struct Slot {
    addr: u64,
    reg: Register,
    /// CLOCK reference bit.
    used: bool,
    live: bool,
    /// A write is in flight into this slot. CLOCK steps over it rather than handing the same
    /// media out twice.
    busy: bool,
}

/// Why a claim found no slot. The metrics keep them apart: `Colder` is the cache working as
/// intended under pressure, `Busy` is transient and self-clearing.
#[derive(Clone, Copy, Debug, PartialEq)]
enum Decline {
    /// Every slot the hand could reach is being written into, or the class holds none.
    Busy,
    /// The entry at the hand is in more demand than the candidate, so it keeps its slot.
    Colder,
}

/// One 4 MiB piece of media the cache holds: either a chunk of the store's tail, which is the
/// cache's own space, or a free 4 MiB data slot on loan from the allocator, which has to go
/// back the moment the slab needs it.
#[derive(Clone, Copy)]
struct Chunk {
    off: u64,
    borrowed: bool,
}

/// Slots one 4 MiB chunk holds for a class: 1024 small, exactly one huge. The asymmetry is why
/// the classes cost such different amounts of DRAM per byte of cache.
fn chunk_slots(class: Class) -> usize {
    (layout::CHUNK_BYTES / class.bytes()) as usize
}

/// This core's share of one class's cache, as a set of 4 MiB chunks.
///
/// Chunks rather than a contiguous stripe because the cache does not own a region carved at
/// format time. It is handed whatever the tail has spare, and it gives pieces back: to the
/// other class when hotness moves, and to the allocator when a borrowed slot is wanted for
/// real data. A chunk is the unit of all three.
struct Store {
    class: Class,
    chunks: Vec<Chunk>,
    /// Slot `i` lives in `chunks[i / chunk_slots]`, at `i % chunk_slots` within it.
    slots: Vec<Slot>,
    /// `GlobalAddr -> local slot`. Grows only when a chunk is added, never on the hot path.
    map: HashMap<u64, u32>,
    hand: u32,
    evicted: u64,
    /// Entries lost because their chunk went back to the other class or the allocator. Kept
    /// apart from `evicted` because the rebalance reads evictions as demand, and counting its
    /// own displacement would make it chase itself.
    dropped: u64,
    /// How many of `chunks` are on loan from the allocator. A running count rather than a
    /// scan: metrics read it on every tick, and `chunks` is as long as the tail is wide.
    borrowed: usize,
}

impl Store {
    fn new(class: Class) -> Store {
        Store {
            class,
            chunks: Vec::new(),
            slots: Vec::new(),
            map: HashMap::new(),
            hand: 0,
            evicted: 0,
            dropped: 0,
            borrowed: 0,
        }
    }

    /// Byte offset of a local slot.
    fn off(&self, i: u32) -> u64 {
        let cs = chunk_slots(self.class);
        let (c, k) = (i as usize / cs, i as usize % cs);
        self.chunks[c].off + k as u64 * self.class.bytes()
    }

    /// Take on another 4 MiB. The slots arrive free, so the only cost is the record and the
    /// map's share of it, which `BYTES_PER_SLOT` estimates.
    fn push_chunk(&mut self, c: Chunk) {
        let cs = chunk_slots(self.class);
        self.borrowed += c.borrowed as usize;
        self.chunks.push(c);
        self.slots.resize(self.slots.len() + cs, Slot::default());
        self.map.reserve(cs);
    }

    /// The slot range chunk `ci` owns.
    fn range(&self, ci: usize) -> std::ops::Range<usize> {
        let cs = chunk_slots(self.class);
        ci * cs..(ci + 1) * cs
    }

    /// Hand back chunk `ci`, dropping whatever it held. `None` while any slot in it, or in the
    /// chunk moved into its place, is being written into: a busy slot has an IO in flight
    /// against those exact bytes, so the write could land on someone else's page.
    fn remove_chunk(&mut self, ci: usize) -> Option<u64> {
        let last = self.chunks.len().checked_sub(1)?;
        if self.busy(ci) || self.busy(last) {
            return None;
        }
        for i in self.range(ci) {
            let s = self.slots[i];
            if self.map.get(&s.addr) == Some(&(i as u32)) {
                self.map.remove(&s.addr);
                if s.live {
                    self.dropped += 1;
                }
            }
        }
        // Swap the last chunk into the hole rather than shifting: a slot index is a position
        // within `chunks`, so the two move together, and moving one chunk's worth of map
        // entries beats moving every chunk after the hole.
        if ci != last {
            let (from, to) = (self.range(last), self.range(ci));
            for (a, b) in from.zip(to) {
                let s = self.slots[a];
                self.slots[b] = s;
                if self.map.get(&s.addr) == Some(&(a as u32)) {
                    self.map.insert(s.addr, b as u32);
                }
            }
        }
        let cs = chunk_slots(self.class);
        self.slots.truncate(self.slots.len() - cs);
        let gone = self.chunks.swap_remove(ci);
        self.borrowed -= gone.borrowed as usize;
        if self.hand as usize >= self.slots.len() {
            self.hand = 0;
        }
        Some(gone.off)
    }

    fn busy(&self, ci: usize) -> bool {
        self.range(ci).any(|i| self.slots[i].busy)
    }

    /// Give back one chunk, preferring `borrowed` ones. Used by the reclaim path, which must
    /// have a borrowed one, and by the rebalance, which must not.
    fn give(&mut self, borrowed: bool) -> Option<u64> {
        let mut ci = self.chunks.len();
        while ci > 0 {
            ci -= 1;
            if self.chunks[ci].borrowed == borrowed
                && let Some(off) = self.remove_chunk(ci)
            {
                return Some(off);
            }
        }
        None
    }

    /// Whether `addr` is already cached at exactly `reg`; re-admitting it would rewrite bytes
    /// already there.
    fn current(&self, addr: u64, reg: Register) -> bool {
        self.map.get(&addr).is_some_and(|&i| {
            let s = &self.slots[i as usize];
            s.live && s.reg == reg
        })
    }

    fn find(&mut self, addr: u64) -> Option<(u32, Register)> {
        let i = *self.map.get(&addr)?;
        let s = &mut self.slots[i as usize];
        if !s.live {
            return None;
        }
        s.used = true;
        Some((i, s.reg))
    }

    fn forget(&mut self, addr: u64) {
        if let Some(i) = self.map.remove(&addr) {
            self.slots[i as usize].live = false;
        }
    }

    /// Claim a slot for `addr`, evicting by CLOCK if need be, and mark it busy for the
    /// duration of the write. Never a reason to wait: both declines mean take the ordinary
    /// read path.
    ///
    /// `hotter(victim)` decides the contest at the hand, and is asked only about a live entry
    /// that has already spent its second chance. An empty slot is taken without one.
    fn claim(
        &mut self,
        addr: u64,
        reg: Register,
        hotter: impl Fn(u64) -> bool,
    ) -> Result<u32, Decline> {
        if self.slots.is_empty() {
            return Err(Decline::Busy);
        }
        let i = match self.map.get(&addr) {
            Some(&i) if !self.slots[i as usize].busy => i,
            Some(_) => return Err(Decline::Busy),
            None => self.evict(hotter)?,
        };
        self.map.insert(addr, i);
        // A fresh entry starts with a clear reference bit: it already passed admission, and a
        // grace period would make the hand step over genuinely hot entries.
        self.slots[i as usize] = Slot {
            addr,
            reg,
            used: false,
            live: false,
            busy: true,
        };
        Ok(i)
    }

    /// The CLOCK hand. Two sweeps at most: the first clears reference bits, the second is
    /// guaranteed to find one clear unless every slot is busy.
    ///
    /// The candidate is measured against the first clear live entry the hand reaches, and
    /// losing to it ends the admission rather than sweeping on for a weaker victim, which
    /// would be O(slots) per admission and would let a scan always find a victim.
    fn evict(&mut self, hotter: impl Fn(u64) -> bool) -> Result<u32, Decline> {
        let n = self.slots.len() as u32;
        for _ in 0..2 * n {
            let i = self.hand;
            self.hand = (self.hand + 1) % n;
            let s = &mut self.slots[i as usize];
            if s.busy {
                continue;
            }
            if !s.live {
                return Ok(i);
            }
            if s.used {
                s.used = false;
                continue;
            }
            let old = s.addr;
            if hotter(old) {
                return Err(Decline::Colder);
            }
            self.slots[i as usize].live = false;
            self.map.remove(&old);
            self.evicted += 1;
            return Ok(i);
        }
        Err(Decline::Busy)
    }

    fn finish(&mut self, i: u32, ok: bool) {
        let s = &mut self.slots[i as usize];
        s.busy = false;
        s.live = ok;
        if !ok {
            self.map.remove(&s.addr);
        }
    }
}

// --- per-core state ---

/// Everything the cache keeps on one core.
///
/// Two shardings meet here. The sketch and the stores are reached on the core owning the
/// address's consensus group, so they ride the hop the round already takes and the sketch sees
/// the whole read stream. The hints are reached on whatever core handles the request, where
/// the reply trailer carrying the width lands; a hint is advisory, so per-core is fine.
pub(crate) struct Local {
    sketch: Sketch,
    stores: [Store; 2],
    hints: Box<[u8]>,
    /// `hints.len() - 1`.
    hint_mask: usize,
    decayed: Instant,
    stats: Stats,
}

impl Local {
    /// Bump one class's counters on the share already open. Taking the share rather than
    /// reaching for it is what lets a transaction count what it just did.
    fn stat(&mut self, huge: bool, f: impl FnOnce(&mut ClassStats)) {
        f(&mut self.stats.per[Cache::class(huge)]);
    }
}

/// Per-core, per-class counters, read by [`Cache::local_stats`]; the exporter sums cores.
#[derive(Clone, Copy, Default)]
pub struct ClassStats {
    pub hits: u64,
    pub misses: u64,
    /// Hits that then passed confirmation against the quorum. The gap against `hits` is what
    /// staleness costs.
    pub served: u64,
    pub admits: u64,
    pub evictions: u64,
    /// Entries lost because their chunk was handed to the other class or back to the
    /// allocator, as opposed to evicted to make room for a hotter page.
    pub dropped: u64,
    pub stale: u64,
    pub shed: u64,
    /// Admissions refused because the extent's `cache_admit` said no: the extent caches
    /// nothing, or the page had not shown the demand it asks for. Counted where the decision
    /// is made, which for 4 KiB pages is the group member computing the width rather than the
    /// node that would have cached. A client seeing few admits and no rejections here is being
    /// vetoed elsewhere.
    pub rejected_policy: u64,
    /// Admissions refused because the entry at the clock hand was in more demand. Rising with
    /// a full cache is the contest doing its job; rising with a cache that is not full means
    /// the class is short of slots, not short of demand.
    pub rejected_victim: u64,
    /// Media this class holds on this core, and the part on loan from the allocator's free
    /// list rather than the store's own tail.
    pub bytes: u64,
    pub borrowed_bytes: u64,
}

/// Counters split by page class. The classes differ by three orders of magnitude in bytes per
/// slot, so one number for hit rate or capacity says nothing about the mix, and the rebalance
/// reads exactly these to decide which class earns more per byte.
#[derive(Clone, Copy, Default)]
pub struct Stats {
    pub per: [ClassStats; 2],
}

// --- Cache ---

/// The tail chunks nobody is holding, plus the bookkeeping the rebalance needs. Exactly one
/// core's [`Row`] has one, and that is the core the rebalance runs on: a core with no pool has
/// no rebalance to start, so being the only writer is a fact about the state rather than a
/// rule about who may reach it.
#[derive(Default)]
pub(crate) struct Pool {
    /// Free tail chunks. Non-empty when the DRAM budget, not the disk, bounds the cache.
    free: Vec<u64>,
    /// Per-class counters at the last sample, for differencing.
    served: [u64; 2],
    evicted: [u64; 2],
    missed: [u64; 2],
    /// When the last sample was taken.
    sampled: Option<Instant>,
    /// Whether a rebalance task is in flight, so `tick` never starts a second one.
    running: bool,
}

/// What one core's stores did with what they hold, gathered by the rebalance.
#[derive(Clone, Copy, Default)]
struct Census {
    served: u64,
    evicted: u64,
    missed: u64,
    slots: u64,
    chunks: u64,
}

pub struct Cache {
    alloc: &'static Allocator,
    disk: Disk,
    /// The tail this cache was handed at open: base and chunk count. Fixed for the life of the
    /// process, because the layout it comes from is.
    tail: (u64, u64),
    /// Cores that can own an address of each class, so how many may hold slots for it.
    shards: [usize; 2],
    /// Slots the node may hold across all cores and classes, from `policy.cache_index_bytes`.
    budget: u64,
}

/// One worker's share of the cache, living in that worker's [`server::CoreState`].
///
/// `pool` is `Some` on exactly one core. The free tail is one list for the whole node and the
/// rebalance that drains it is a single task, so rather than a shared list and a rule about
/// who may touch it, the core that runs the rebalance is the core that has one to run.
pub(crate) struct Row {
    pub(crate) local: RefCell<Local>,
    pub(crate) pool: Option<RefCell<Pool>>,
}

/// Build the cache and leak it, with one row per worker for the runtime to hand out.
///
/// The cache is entitled to the whole tail, every 4 MiB the layout did not claim, subject to
/// what `policy.cache_index_bytes` will pay for in slot records. A store with no room past its
/// slabs, or a config no extent opts into, produces a cache with no slots, which declines
/// everything at no cost.
pub fn open(alloc: &'static Allocator, cfg: &Config, cores: usize) -> (&'static Cache, Vec<Row>) {
    let geo = alloc.geometry();
    let (base, _) = geo.tail(cfg.node.store_bytes);
    let chunks = geo.tail_chunks(cfg.node.store_bytes);
    let budget = cfg.policy.cache_index_bytes / BYTES_PER_SLOT;
    // Only cores an address of this class can hop to. `owner_core` routes every lookup through
    // `shards_for`, so slots placed on a core past that are unreachable.
    let shards = [Class::Small, Class::Huge].map(|c| alloc.shards_for(c).min(cores).max(1));

    let want = plan_chunks(chunks, budget / OPEN_SHARE, cfg);
    let now = crate::runtime::now();
    let mut next = [0u64, want[0]];
    // Sized from what a core may end up holding, not from what it starts with: both tables are
    // approximations whose error is what they cost, and resizing either would throw away the
    // history that makes them worth keeping.
    let share = (budget / cores as u64).max(1);
    let (cols, hints) = (
        table_len(share, COLS_MIN, COLS_MAX),
        table_len(share, HINTS_MIN, HINTS_MAX),
    );

    let state: Vec<Local> = (0..cores)
        .map(|c| {
            let stores = [Class::Small, Class::Huge].map(|class| {
                let k = usize::from(class == Class::Huge);
                let mut s = Store::new(class);
                if c < shards[k] {
                    let (lo, len) = stripe(want[k], shards[k], c);
                    for i in 0..len {
                        s.push_chunk(Chunk {
                            off: base + (next[k] + lo + i) * layout::CHUNK_BYTES,
                            borrowed: false,
                        });
                    }
                }
                s
            });
            Local {
                sketch: Sketch::new(cols),
                hints: vec![0u8; hints].into_boxed_slice(),
                hint_mask: hints - 1,
                stores,
                decayed: now + phase(c, cores),
                stats: Stats::default(),
            }
        })
        .collect();

    next[1] += want[1];
    let mut free = Some(
        (next[1]..chunks)
            .map(|i| base + i * layout::CHUNK_BYTES)
            .collect(),
    );

    let rows = state
        .into_iter()
        .enumerate()
        .map(|(c, local)| Row {
            local: RefCell::new(local),
            // The rebalance is one task for the node, so exactly one core is handed the pool
            // it works from; every other core's row simply has none.
            pool: (c == 0).then(|| {
                RefCell::new(Pool {
                    free: free.take().expect("one core holds the pool"),
                    ..Pool::default()
                })
            }),
        })
        .collect();

    let cache = Box::leak(Box::new(Cache {
        alloc,
        disk: alloc.disk(),
        tail: (base, chunks),
        shards,
        budget,
    }));
    (cache, rows)
}

/// How many of `chunks` each class starts with.
///
/// The split is the node's own byte mix, on the theory that a node holding mostly 4 MiB pages
/// has peers reading mostly 4 MiB pages; a pure cache client, holding neither, splits evenly.
/// The rebalance then moves chunks toward whichever class earns hits, so this only has to be a
/// decent prior. `budget` binds the small class and effectively never the huge one: a 4 MiB
/// chunk is 1024 small slots or one huge slot. Chunks the budget cannot pay for stay in the
/// pool rather than going to huge, so a later shift toward small can draw on them without
/// taking media from huge.
fn plan_chunks(chunks: u64, budget: u64, cfg: &Config) -> [u64; 2] {
    if chunks == 0 {
        return [0, 0];
    }
    let sb = cfg.small_pages() * layout::SMALL_PAGE;
    let hb = cfg.huge_pages() * layout::HUGE_PAGE;
    let mut want = match (chunks * sb).checked_div(sb + hb) {
        Some(s) => [s, chunks - s],
        None => [chunks / 2, chunks - chunks / 2],
    };
    want[0] = want[0].min(budget / chunk_slots(Class::Small) as u64);
    want[1] = want[1].min(budget.saturating_sub(want[0] * chunk_slots(Class::Small) as u64));
    want
}

/// Which class the next chunk should go to, if either.
///
/// Two signals, both over one interval. Evictions are demand: a class that has not had to
/// reuse a slot never asks. Confirmed hits per byte held is value, per byte because bytes are
/// the currency handed out. `REBALANCE_MARGIN` is the hysteresis against noise.
fn wants(served: [u64; 2], evicted: [u64; 2], missed: [u64; 2], bytes: [u64; 2]) -> Option<usize> {
    // A class holding nothing cannot evict, so its misses stand in for its demand. Without
    // this a class that started empty could never be given anything.
    let short = |k: usize| evicted[k] > 0 || (bytes[k] == 0 && missed[k] > 0);
    match (short(0), short(1)) {
        (false, false) => None,
        (true, false) => Some(0),
        (false, true) => Some(1),
        (true, true) => {
            // A class holding nothing has no measured yield and takes the tie: one chunk is
            // what it costs to find out whether it deserves more.
            if bytes[0] == 0 {
                return Some(0);
            }
            if bytes[1] == 0 {
                return Some(1);
            }
            // served[k] / bytes[k], cross multiplied to stay in integers.
            let a = served[0].saturating_mul(bytes[1]);
            let b = served[1].saturating_mul(bytes[0]);
            if a > b.saturating_mul(REBALANCE_MARGIN) {
                Some(0)
            } else if b > a.saturating_mul(REBALANCE_MARGIN) {
                Some(1)
            } else {
                None
            }
        }
    }
}

/// Hysteresis: raise fast, lower one step at a time. Rendezvous nesting means a one-step
/// change churns only the boundary replica, so damping the descent is the whole
/// anti-oscillation story.
fn damp(cur: u8, w: u8) -> u8 {
    if w >= cur {
        w
    } else {
        cur.saturating_sub(1).max(w)
    }
}

/// Core `c`'s contiguous share of `total` chunks, as `(first, count)`. Contiguous rather than
/// round-robin so a CLOCK sweep walks the device in order.
fn stripe(total: u64, cores: usize, c: usize) -> (u64, u64) {
    let lo = total * c as u64 / cores as u64;
    let hi = total * (c + 1) as u64 / cores as u64;
    (lo, hi - lo)
}

/// Where in the decay interval core `c` does its halving. Every core is handed the same start
/// instant and then advances in whole `DECAY` steps, so without an offset they all halve on
/// the same boundary and a node walks `cores` whole sketches at once. The offset is added
/// rather than subtracted so it cannot underflow an `Instant` after a fresh boot.
fn phase(c: usize, cores: usize) -> Duration {
    DECAY * c as u32 / cores.max(1) as u32
}

/// This core's share of the cache.
fn here<T>(f: impl FnOnce(&mut Local) -> T) -> T {
    runtime::here::<Server, T>(|ctx| f(&mut ctx.state().cache.local.borrow_mut()))
}

/// `core`'s share of the cache, as a transaction that core runs.
///
/// The whole body is synchronous, which is the point: a lookup, a claim or a hand-back is one
/// visit to the owning worker rather than a task parked on it.
fn at<T, F>(core: CoreId, f: F) -> impl Future<Output = T>
where
    F: FnOnce(&mut Local) -> T + Send + 'static,
    T: Send + 'static,
{
    runtime::with_core::<Server, _, _>(core, move |ctx| {
        f(&mut ctx.state().cache.local.borrow_mut())
    })
}

/// The free tail, on the one core that has it; `None` anywhere else.
fn pool<T>(f: impl FnOnce(&mut Pool) -> T) -> Option<T> {
    runtime::here::<Server, _>(|ctx| {
        ctx.state()
            .cache
            .pool
            .as_ref()
            .map(|p| f(&mut p.borrow_mut()))
    })
}

impl Cache {
    /// The core owning `addr` for its class, named rather than indexed.
    fn owner_of(&self, addr: GlobalAddr) -> Option<CoreId> {
        let c = self.alloc.owner_core(addr).ok()?;
        Some(CoreId::new(c).expect("an owner is a worker"))
    }

    /// Whether this node can cache at all. Structural only, and deliberately cheap: a cohort
    /// of nobody leaves no peer to place a replica on, and a config where no extent opts in
    /// leaves nothing to place. Both are properties of the config rather than the address, so
    /// a lookup can be refused before it costs a hop. Whether a particular page should be
    /// cached is per extent and lives in `observe_local`.
    fn enabled(&self) -> bool {
        let r = server::roster();
        r.admits && r.widest() > 0
    }

    /// Sheds while the allocator is short of free space, or while the store's rate budget is
    /// committed ahead. Cache space is statically separate from the allocator's but store
    /// bandwidth is not, so the cache stops admitting before anything authoritative slows
    /// down. An inbound extent migration reaches the cache through this alone.
    pub fn shedding(&self) -> bool {
        self.alloc.pressure() != Pressure::Normal || self.alloc.store_pressed()
    }

    fn stat(&self, huge: bool, f: impl FnOnce(&mut ClassStats)) {
        here(|l| l.stat(huge, f));
    }

    /// This core's counters. Evictions, capacity and the borrowed share are the stores' own
    /// state rather than separate counters.
    pub fn local_stats(&self) -> Stats {
        here(|l| {
            let mut s = l.stats;
            for (k, st) in l.stores.iter().enumerate() {
                s.per[k].evictions = st.evicted;
                s.per[k].dropped = st.dropped;
                s.per[k].bytes = st.chunks.len() as u64 * layout::CHUNK_BYTES;
                s.per[k].borrowed_bytes = st.borrowed as u64 * layout::CHUNK_BYTES;
            }
            s
        })
    }

    /// Count a cached page that passed confirmation. The one counter the cache cannot keep
    /// itself, because confirmation happens inside the consensus round.
    pub fn served(&self, huge: bool) {
        self.stat(huge, |s| s.served += 1);
    }

    // --- hotness ---

    /// Record one read of `addr` and return the width its owner should advertise, zero unless
    /// the extent's `cache_admit` is satisfied.
    ///
    /// Must run on the core owning the address's group: paxos already handles the metadata
    /// round there, so the owner observes the whole read stream even for pages it no longer
    /// serves, and for a 4 KiB page it is the *only* node that sees every read, because the
    /// nodes that would cache it are by construction not group members. Zero width is the
    /// veto: `holds` and `replica` refuse it and the reader's `offer` never calls `admit`, so
    /// the threshold needs no wire field and no extra round; it rides in the reply trailer
    /// that carries `w`.
    pub fn observe_local(&self, addr: GlobalAddr) -> u8 {
        here(|l| self.observe_in(l, addr))
    }

    /// [`Self::observe_local`] against a share already open, so the counters it bumps and the
    /// sketch it feeds are the same borrow.
    fn observe_in(&self, l: &mut Local, addr: GlobalAddr) -> u8 {
        // Nothing to advertise a width to, so nothing to count either: a node whose cache is
        // off by config would otherwise report every read as a rejection.
        if !self.enabled() {
            return 0;
        }
        // One lookup for both the class and the threshold; an address in no extent of ours is
        // not a rejection, it is nothing to reject.
        let cfg = server::config();
        let Some((huge, n)) = cfg.extent_at(addr.0).map(|e| (e.huge, e.cache_admit)) else {
            return 0;
        };
        if n == 0 {
            l.stat(huge, |s| s.rejected_policy += 1);
            return 0;
        }
        let cap = self.w_max();
        // Observe before testing, so a page below the threshold still accumulates the demand
        // that would carry it over. `n == 1` therefore always passes: the sketch never answers
        // below one for a key it has just seen.
        let q = l.sketch.observe(addr.0);
        if q < n {
            l.stat(huge, |s| s.rejected_policy += 1);
            return 0;
        }
        q.min(cap)
    }

    /// `observe_local` with a hop of its own, for the 4 MiB path: an immutable hit takes no
    /// metadata round, so no owner reply carries a width and the reader's own read stream is
    /// the only signal.
    pub async fn observe(&'static self, addr: GlobalAddr) -> u8 {
        if !self.enabled() {
            return 0;
        }
        let Some(owner) = self.owner_of(addr) else {
            return 0;
        };
        at(owner, move |l| self.observe_in(l, addr)).await
    }

    /// `W_max = min(cohort_size, 64)`.
    fn w_max(&self) -> u8 {
        u8::try_from(server::roster().widest())
            .unwrap_or(W_CAP)
            .min(W_CAP)
    }

    // --- hints ---

    /// The width last advertised for `addr`, as this core remembers it. The cached leg and the
    /// metadata round are issued together, but `w` arrives *in* that round's reply, so the leg
    /// can only be taken on a width learned from an earlier read. The first read of a key is
    /// therefore always uncached, which is what the admission filter wants.
    pub fn hint(&self, addr: GlobalAddr) -> u8 {
        here(|l| l.hints[config::mix(addr.0) as usize & l.hint_mask])
    }

    /// Record the width from a reply trailer, damped.
    pub fn note_hint(&self, addr: GlobalAddr, w: u8) {
        here(|l| {
            let i = config::mix(addr.0) as usize & l.hint_mask;
            l.hints[i] = damp(l.hints[i], w);
        });
    }

    // --- placement ---

    /// Whether we are one of the `w` replicas for `addr`: the number of cohort peers that
    /// outrank us is below the width.
    pub fn holds(&self, addr: GlobalAddr, w: u8) -> bool {
        let r = server::roster();
        let nodes = r.cohort(addr.universe());
        if w == 0 || nodes.is_empty() {
            return false;
        }
        let mine = rank(addr.0, r.me);
        nodes.iter().filter(|&&n| rank(addr.0, n) > mine).count() < w as usize
    }

    /// The highest-ranked live member of `R`, excluding ourselves. `ok` is the reachability
    /// test: a cohort peer we hold no link to is not a candidate.
    pub fn replica(&self, addr: GlobalAddr, w: u8, ok: impl Fn(u32) -> bool) -> Option<u32> {
        let r = server::roster();
        let nodes = r.cohort(addr.universe());
        if w == 0 {
            return None;
        }
        let mut best: Option<(u64, u32)> = None;
        for &n in nodes {
            if n == r.me || !ok(n) {
                continue;
            }
            let s = rank(addr.0, n);
            if best.is_none_or(|(b, _)| s > b) {
                best = Some((s, n));
            }
        }
        let (score, node) = best?;
        let ahead = nodes.iter().filter(|&&n| rank(addr.0, n) > score).count();
        (ahead < w as usize).then_some(node)
    }

    // --- store ---

    fn class(huge: bool) -> usize {
        usize::from(huge)
    }

    /// Read `addr` out of the local cache region into `buf`, returning the register the entry
    /// claims. The caller confirms that register against the quorum; this makes no claim about
    /// freshness. `off` is a byte offset into the page, for the 4 MiB class where one page is
    /// many block requests.
    pub async fn load(
        &'static self,
        addr: GlobalAddr,
        huge: bool,
        off: usize,
        buf: Buf,
    ) -> Option<Register> {
        self.load_at(addr, huge, off, buf, None).await
    }

    /// The cached 4 MiB path. An entry is servable only while it carries the current epoch's
    /// live version, so an epoch bump invalidates every immutable entry with one comparison;
    /// that is the whole invalidation protocol. The register comes back for the caller to
    /// confirm against the group: this class has no per-page checksum and a 4 MiB frame has no
    /// trailer, so the version is the only handle on these bytes.
    pub async fn load_immutable(
        &'static self,
        addr: GlobalAddr,
        huge: bool,
        off: usize,
        buf: Buf,
    ) -> Option<Register> {
        if !self.enabled() {
            return None;
        }
        self.load_at(addr, huge, off, buf, Some(self.live_version(addr)))
            .await
    }

    /// What our cached copy of `addr` claims, with no bytes moved: the register half of a 4
    /// MiB cache hit, whose frame has no trailer to gather one into. Filtered exactly as
    /// [`Self::load_immutable`] filters, so the two agree on which entry they mean.
    pub async fn peek_immutable(&'static self, addr: GlobalAddr, huge: bool) -> Option<Register> {
        if !self.enabled() {
            return None;
        }
        let want = self.live_version(addr);
        let owner = self.owner_of(addr)?;
        at(owner, move |l| {
            self.find_in(l, addr, huge, Some(want)).map(|(_, r)| r)
        })
        .await
    }

    /// Look the entry up on the core that owns it, then read the bytes here.
    ///
    /// The lookup has to hop and the IO must not: a registered buffer's index is meaningful
    /// only on the ring it was registered on, so `buf` may only be handed to the disk from the
    /// core the request arrived on. Same shape as the allocator's reserve-here, write-there
    /// split. `want` filters on the version *before* any IO, so a stale immutable entry never
    /// puts bytes in the caller's buffer.
    async fn load_at(
        &'static self,
        addr: GlobalAddr,
        huge: bool,
        off: usize,
        buf: Buf,
        want: Option<u64>,
    ) -> Option<Register> {
        let owner = self.owner_of(addr)?;
        let (found, reg) = at(owner, move |l| self.find_in(l, addr, huge, want)).await?;
        let class = Class::of(huge);
        if off as u64 + buf.len() as u64 > class.bytes() {
            return None;
        }
        if self.disk.read(found + off as u64, buf).await.is_err() {
            // A cache page we cannot read is one we do not have. Silent rot is invisible here
            // (confirmation covers the register, not the bytes), but a hard read error means
            // the entry is gone.
            at(owner, move |l| {
                l.stores[Cache::class(huge)].forget(addr.0);
                l.stat(huge, |s| s.misses += 1);
            })
            .await;
            return None;
        }
        self.stat(huge, |s| s.hits += 1);
        Some(reg)
    }

    /// The absolute byte offset of `addr`'s entry, resolved on the owning core because only
    /// that core knows which chunks its store holds. The caller does the IO, so it gets an
    /// offset and not a slot: by the time the bytes move, the slot number could mean a
    /// different chunk.
    fn find_in(
        &self,
        l: &mut Local,
        addr: GlobalAddr,
        huge: bool,
        want: Option<u64>,
    ) -> Option<(u64, Register)> {
        let k = Cache::class(huge);
        let found = l.stores[k].find(addr.0);
        let Some((slot, reg)) = found.filter(|v| want.is_none_or(|w| v.1.version == w)) else {
            l.stat(huge, |s| s.misses += 1);
            return None;
        };
        // A hit is a read too, and for a 4 KiB page this is the only place this node learns of
        // one: group members see the metadata round, but a hit served from here never reaches
        // them, so a resident entry's estimate would only decay and lose the contest to the
        // first candidate walking past. The 4 MiB path counted this read in `cache_width`
        // already.
        if !huge {
            l.sketch.observe(addr.0);
        }
        Some((l.stores[k].off(slot), reg))
    }

    /// Offer `buf` to the cache as the value of `addr` at `reg`, given the width `w` its owner
    /// last advertised. Declines silently: not one of the `w` replicas, not hot enough, device
    /// under pressure, or every slot being written. The cache never fails a write, it declines
    /// one.
    pub async fn admit(
        &'static self,
        addr: GlobalAddr,
        huge: bool,
        buf: Buf,
        reg: Register,
        w: u8,
    ) {
        if !self.enabled() || !self.holds(addr, w) {
            return;
        }
        let Some(owner) = self.owner_of(addr) else {
            return;
        };
        // Claim on the owning core, write here, then report back: the buffer's registration
        // belongs to this core's ring and cannot travel (see `load_at`).
        let Some((slot, dst)) = at(owner, move |l| self.claim_in(l, addr, huge, reg)).await else {
            return;
        };
        // One IO, in place, `Buffered`: no torn-write hazard, because nothing points at these
        // bytes after a restart. The slot stays busy across it, which also stops its chunk
        // from being handed away underneath the write.
        let ok = buf.len() as u64 == Class::of(huge).bytes()
            && self
                .disk
                .write(dst, buf, Durability::Buffered)
                .await
                .is_ok();
        at(owner, move |l| {
            l.stores[Cache::class(huge)].finish(slot, ok);
            if ok {
                l.stat(huge, |s| s.admits += 1);
            }
        })
        .await;
    }

    /// Take a slot for `addr` on the owning core, or decline.
    ///
    /// The threshold is not rechecked here: `observe_local` already applied it, and the zero
    /// width it returns on a veto never reaches this function. The kill switch is rechecked,
    /// because it has to bite the moment the config lands rather than after the reader's
    /// damped width hint drains; it costs one extent lookup and no sketch.
    fn claim_in(
        &self,
        l: &mut Local,
        addr: GlobalAddr,
        huge: bool,
        reg: Register,
    ) -> Option<(u32, u64)> {
        let k = Cache::class(huge);
        if l.stores[k].current(addr.0, reg) {
            return None;
        }
        if self.shedding() {
            l.stat(huge, |s| s.shed += 1);
            return None;
        }
        if server::config().cache_admit_of(addr.0) == 0 {
            l.stat(huge, |s| s.rejected_policy += 1);
            return None;
        }
        // Count the read this node is about to serve from its own cache. The 4 MiB path
        // already did so in `cache_width`, on this same core; the 4 KiB path has not, because
        // for a small page `observe_local` runs on a group member and this node is not one.
        // Otherwise the local sketch answers zero for every small page and the contest below
        // is a coin toss it always wins.
        let cand = if huge {
            l.sketch.estimate(addr.0)
        } else {
            l.sketch.observe(addr.0)
        };
        let (sketch, store) = (&l.sketch, &mut l.stores[k]);
        // The contest, and the only place hotness reaches victim selection. Ties go to the
        // candidate, so a cold scan at an estimate of one churns other cold entries and leaves
        // anything at two or more alone.
        let slot = match store.claim(addr.0, reg, |victim| sketch.estimate(victim) > cand) {
            Ok(slot) => slot,
            Err(Decline::Colder) => {
                l.stats.per[k].rejected_victim += 1;
                return None;
            }
            Err(Decline::Busy) => return None,
        };
        Some((slot, l.stores[k].off(slot)))
    }

    /// Drop an entry that failed confirmation. Cheap enough to call speculatively.
    pub async fn forget(&'static self, addr: GlobalAddr, huge: bool) {
        let Some(owner) = self.owner_of(addr) else {
            return;
        };
        at(owner, move |l| {
            l.stores[Cache::class(huge)].forget(addr.0);
            l.stat(huge, |s| s.stale += 1);
        })
        .await;
    }

    // --- immutable ---

    /// The version an Immutable page holds while it is live at its extent's epoch. Only
    /// quorum-confirmed values are admitted for this class, and one version has exactly one
    /// such value, so a version is a complete identity for a cached 4 MiB page. No ballot
    /// beside it, which matters because a 4 MiB frame has no trailer.
    fn live_version(&self, addr: GlobalAddr) -> u64 {
        3 * server::config().tombstone_epoch_of(addr.0) + 1
    }

    // --- tick ---

    /// The decay, and where the pool is the rebalance. Driven from `Handler::tick`, which is a
    /// poll and not a timer: an idle worker takes no ticks, so the halving count comes from
    /// elapsed time rather than assumed to be one.
    pub fn tick(&'static self, now: Instant) {
        here(|l| {
            let elapsed = now.saturating_duration_since(l.decayed);
            let steps = (elapsed.as_nanos() / DECAY.as_nanos()) as u32;
            if steps > 0 {
                l.decayed += DECAY * steps;
                l.sketch.halve(steps);
            }
        });
        // The rebalance has to see every core, so it hops and cannot run from a synchronous
        // tick. The core holding the pool spawns it; the flag keeps a slow one from being
        // started twice. Every other core has no pool, and so nothing here to start.
        let start = pool(|p| {
            let due = p
                .sampled
                .is_none_or(|t| now.saturating_duration_since(t) >= REBALANCE);
            if p.running || !due {
                return false;
            }
            p.sampled = Some(now);
            p.running = true;
            true
        });
        if start != Some(true) {
            return;
        }
        if !runtime::spawn(async move {
            self.rebalance().await;
            pool(|p| p.running = false);
        }) {
            pool(|p| p.running = false);
        }
    }

    // --- rebalance ---

    /// The tail this cache was given, and the part of it no class is holding. Answers on the
    /// core the pool lives on, and zero unheld elsewhere. Unheld bytes are media the store has
    /// and `policy.cache_index_bytes` would not pay to index, so this says whether raising
    /// that policy would buy anything.
    pub fn tail_bytes(&self) -> (u64, u64) {
        let idle = pool(|p| p.free.len() as u64).unwrap_or(0);
        (
            self.tail.1 * layout::CHUNK_BYTES,
            idle * layout::CHUNK_BYTES,
        )
    }

    /// One core's contribution to the rebalance's view, per class.
    fn census_in(l: &Local) -> [Census; 2] {
        std::array::from_fn(|k| Census {
            served: l.stats.per[k].served,
            evicted: l.stores[k].evicted,
            missed: l.stats.per[k].misses,
            slots: l.stores[k].slots.len() as u64,
            chunks: l.stores[k].chunks.len() as u64,
        })
    }

    /// Move media toward whichever class is earning more with what it has. Spawned from
    /// `tick`, on the core holding the pool.
    ///
    /// Free chunks from the pool go out in batches, because nothing is lost by filling space
    /// no one holds and a large tail would otherwise take hours to warm. Taking media *from*
    /// the other class is one chunk at a time: a steal drops whatever that chunk held, so a
    /// fast controller loses more entries than a slow one gains in fit.
    async fn rebalance(&'static self) {
        let cores = runtime::cores();
        let mut sum = [Census::default(); 2];
        let mut chunks = [
            Vec::<u64>::with_capacity(cores),
            Vec::<u64>::with_capacity(cores),
        ];
        for c in 0..cores {
            let c = CoreId::new(c).expect("a worker index is a worker");
            let row = at(c, |l| Cache::census_in(l)).await;
            for k in 0..2 {
                sum[k].served += row[k].served;
                sum[k].evicted += row[k].evicted;
                sum[k].missed += row[k].missed;
                sum[k].slots += row[k].slots;
                chunks[k].push(row[k].chunks);
            }
        }

        // Counters are cumulative; what the classes did with what they hold *now* is the
        // difference against the last sample.
        let Some((served, evicted, missed)) = pool(|p| {
            let d = |now: [u64; 2], then: &mut [u64; 2]| -> [u64; 2] {
                let out = std::array::from_fn(|k| now[k].saturating_sub(then[k]));
                *then = now;
                out
            };
            let now = |f: fn(&Census) -> u64| [f(&sum[0]), f(&sum[1])];
            (
                d(now(|c| c.served), &mut p.served),
                d(now(|c| c.evicted), &mut p.evicted),
                d(now(|c| c.missed), &mut p.missed),
            )
        }) else {
            return;
        };

        let bytes: [u64; 2] =
            std::array::from_fn(|k| chunks[k].iter().sum::<u64>() * layout::CHUNK_BYTES);
        let Some(k) = wants(served, evicted, missed, bytes) else {
            return;
        };
        let other = 1 - k;
        let cost = chunk_slots(Class::of(k == 1)) as u64;
        let refund = chunk_slots(Class::of(other == 1)) as u64;
        let held = sum[0].slots + sum[1].slots;

        // Tail nobody holds costs no class anything, so it goes first, and in a batch: the
        // cache opens on a fraction of its budget and this is how it earns the rest.
        if held + cost <= self.budget {
            let room = (self.budget - held) / cost;
            let idle: Vec<u64> = pool(|p| {
                let grow = (chunks[k].iter().sum::<u64>() / GROWTH_SHARE)
                    .clamp(1, room)
                    .min(p.free.len() as u64);
                let n = p.free.len() - grow as usize;
                p.free.split_off(n)
            })
            .unwrap_or_default();
            if !idle.is_empty() {
                for off in idle {
                    self.place(
                        k,
                        &mut chunks[k],
                        Chunk {
                            off,
                            borrowed: false,
                        },
                    )
                    .await;
                }
                return;
            }
        }
        // Then a free 4 MiB data slot on loan from the allocator. Huge only: a borrowed chunk
        // is one huge entry, so giving it back drops one page, where a borrowed small chunk
        // would drop 1024 at once.
        if k == 1 && held + cost <= self.budget && self.borrow(&mut chunks[1]).await {
            return;
        }
        // Last, take one from the other class. This is also the only way the small class grows
        // once the DRAM budget is spent, since a 4 MiB chunk of huge frees one slot and a
        // chunk of small costs 1024.
        if held.saturating_sub(refund) + cost <= self.budget
            && let Some(off) = self.take(other, &mut chunks[other]).await
        {
            self.place(
                k,
                &mut chunks[k],
                Chunk {
                    off,
                    borrowed: false,
                },
            )
            .await;
        }
    }

    /// Cores that can own an address of class `k`, ordered by how many chunks of it they hold:
    /// fewest first when `most` is false. Evening the classes out across cores matters because
    /// group hashing spreads addresses evenly, and an unbalanced core would evict while its
    /// neighbour idled.
    fn order(&self, k: usize, chunks: &[u64], most: bool) -> Vec<CoreId> {
        let mut v: Vec<CoreId> = (0..self.shards[k])
            .map(|c| CoreId::new(c).expect("a shard is a worker"))
            .collect();
        v.sort_by_key(|&c| {
            let n = chunks.get(c.index()).copied().unwrap_or(0);
            if most { u64::MAX - n } else { n }
        });
        v
    }

    /// Give a chunk to the class's store on the core holding the fewest, and record it, so a
    /// batch spreads instead of piling onto one core.
    async fn place(&'static self, k: usize, chunks: &mut [u64], c: Chunk) {
        let order = self.order(k, chunks, false);
        let Some(&core) = order.first() else {
            return;
        };
        at(core, move |l| l.stores[k].push_chunk(c)).await;
        chunks[core.index()] += 1;
    }

    /// Take one chunk back from a class, from the core holding the most that will part with
    /// one. `None` when every chunk it holds is borrowed or has a write in flight.
    async fn take(&'static self, k: usize, chunks: &mut [u64]) -> Option<u64> {
        for core in self.order(k, chunks, true) {
            let got = at(core, move |l| l.stores[k].give(false)).await;
            if got.is_some() {
                chunks[core.index()] -= 1;
                return got;
            }
        }
        None
    }

    /// Borrow one free 4 MiB data slot and give it to the huge store on the core that lent it.
    /// The lend and the push happen in one hop, so a slot is never on loan to nobody.
    ///
    /// The same core on purpose: the allocator takes a loan back synchronously, from inside a
    /// reservation that cannot await, so the loan must sit where its shard can reach it
    /// without a hop. That works out, because a core owning a stripe of the 4 MiB slab owns a
    /// stripe of the 4 MiB cache too: both are `mblock % cores` over the same mblocks.
    async fn borrow(&'static self, chunks: &mut [u64]) -> bool {
        for core in self.order(1, chunks, false) {
            let got = at(core, move |l| {
                let off = self.alloc.lend()?;
                l.stores[1].push_chunk(Chunk {
                    off,
                    borrowed: true,
                });
                Some(())
            })
            .await;
            if got.is_some() {
                chunks[core.index()] += 1;
                return true;
            }
        }
        false
    }

    /// Hand one borrowed chunk back, dropping the page it held. `None` when this core holds no
    /// loan it can part with.
    ///
    /// Called synchronously by the allocator, on the core that lent it, from inside a
    /// reservation. It takes no core to call: a loan only ever sits in the share of the core
    /// that made it, which is the core asking. Touches this core's huge store and nothing
    /// else: no config, no pressure test, no hop, because the allocator has state borrowed
    /// around the call.
    pub fn give_back(&self) -> Option<u64> {
        here(|l| l.stores[1].give(true))
    }
}

// --- tests ---

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sketch_is_one_sided_and_decays() {
        let mut s = Sketch::new(COLS_MIN);
        for _ in 0..5 {
            s.observe(42);
        }
        // Conservative update never under-counts, and cannot exceed what was fed in.
        assert_eq!(s.estimate(42), 5);
        assert_eq!(s.estimate(43), 0);

        // Saturation is a ceiling, not a wrap.
        for _ in 0..100 {
            s.observe(42);
        }
        assert_eq!(s.estimate(42), COUNTER_MAX);

        s.halve(1);
        assert_eq!(s.estimate(42), COUNTER_MAX / 2);
        s.halve(4);
        assert_eq!(s.estimate(42), 0);
    }

    #[test]
    fn nibbles_do_not_bleed() {
        let mut s = Sketch::new(COLS_MIN);
        // Two adjacent cells in row 0, forced by construction rather than by hashing.
        s.set(0, 0, 15);
        s.set(0, 1, 0);
        s.halve(1);
        assert_eq!(s.get(0, 0), 7);
        assert_eq!(s.get(0, 1), 0);
    }

    #[test]
    fn halve_matches_repeated_halving() {
        // The word-at-a-time shift and mask must match the obvious loop everywhere, including
        // past the point where a counter has nothing left to give.
        for n in 0..=6u32 {
            let (mut fast, mut slow) = (Sketch::new(COLS_MIN), Sketch::new(COLS_MIN));
            for i in 0..64usize {
                for r in 0..ROWS {
                    let v = ((i + r) % 16) as u8;
                    fast.set(r, i, v);
                    slow.set(r, i, v);
                }
            }
            fast.halve(n);
            for row in &mut slow.rows {
                for b in row.iter_mut() {
                    for _ in 0..n {
                        *b = (*b >> 1) & 0x77;
                    }
                }
            }
            assert_eq!(fast.rows, slow.rows, "n={n}");
        }
    }

    #[test]
    fn a_short_row_still_halves_its_tail() {
        // Four bytes to a row, so the word loop never runs and only the remainder is used.
        let mut s = Sketch::new(8);
        for i in 0..8 {
            s.set(0, i, (i * 2) as u8);
        }
        s.halve(1);
        for i in 0..8 {
            assert_eq!(s.get(0, i), i as u8);
        }
    }

    #[test]
    fn a_sketch_nobody_observes_stops_decaying() {
        let mut s = Sketch::new(COLS_MIN);
        for _ in 0..100 {
            s.observe(7);
        }
        assert_eq!(s.estimate(7), COUNTER_MAX);

        for i in 1..=ZEROED_AFTER {
            s.halve(1);
            assert_eq!(s.since, i);
        }
        assert_eq!(s.estimate(7), 0);

        // Provably all zeroes now, so further decay is refused rather than walked.
        s.halve(1);
        assert_eq!(s.since, ZEROED_AFTER);
    }

    #[test]
    fn a_saturated_key_keeps_decaying() {
        let mut s = Sketch::new(COLS_MIN);
        for _ in 0..100 {
            s.observe(9);
        }
        assert_eq!(s.estimate(9), COUNTER_MAX);

        // A key at the ceiling stops raising counters, so the reads keeping it hot no longer
        // refresh the zero check. Decay must still reach it: the check can only trip after a
        // full run of halvings, by which point the counter is gone anyway.
        for _ in 0..8 {
            s.observe(9);
        }
        for want in [7, 3, 1, 0] {
            s.halve(1);
            assert_eq!(s.estimate(9), want);
        }
    }

    #[test]
    fn cores_decay_on_their_own_phase() {
        // Core zero keeps its boundary; the rest spread across the interval so a node does not
        // walk every sketch it owns at once.
        let cores = 6;
        let mut last = Duration::ZERO;
        for c in 0..cores {
            let p = phase(c, cores);
            assert!(p < DECAY, "c={c}");
            assert!(c == 0 || p > last, "c={c}");
            last = p;
        }
        assert_eq!(phase(0, cores), Duration::ZERO);
        // One core has nobody to spread away from, and no divisor to trip over.
        assert_eq!(phase(0, 0), Duration::ZERO);
    }

    /// Widening the replica set only ever appends to it.
    #[test]
    fn rendezvous_nests() {
        let nodes: Vec<u32> = (1..=40).collect();
        let top = |addr: u64, w: usize| -> Vec<u32> {
            let mut v: Vec<(u64, u32)> = nodes.iter().map(|&n| (rank(addr, n), n)).collect();
            v.sort_unstable_by(|a, b| b.cmp(a));
            v.truncate(w);
            v.into_iter().map(|(_, n)| n).collect()
        };
        for addr in 0..64u64 {
            for w in 1..8 {
                let a = top(addr, w);
                let b = top(addr, w + 1);
                assert_eq!(a, b[..w], "R(k,{w}) must prefix R(k,{})", w + 1);
            }
        }
    }

    /// Placement must spread, or caching buys nothing.
    #[test]
    fn rendezvous_spreads() {
        let nodes: Vec<u32> = (1..=8).collect();
        let mut hits = [0usize; 9];
        for addr in 0..4096u64 {
            let best = nodes
                .iter()
                .copied()
                .max_by_key(|&n| rank(addr, n))
                .unwrap();
            hits[best as usize] += 1;
        }
        for n in &nodes {
            let h = hits[*n as usize];
            assert!(h > 4096 / 16, "node {n} took only {h} of 4096");
        }
    }

    impl Store {
        /// `claim` with no contest; the contest has tests of its own.
        fn claim_uncontested(&mut self, addr: u64, reg: Register) -> Result<u32, Decline> {
            self.claim(addr, reg, |_| false)
        }
    }

    /// A store holding `n` chunks of tail, laid out end to end from a plausible base.
    fn store(class: Class, n: u64) -> Store {
        let mut st = Store::new(class);
        for i in 0..n {
            st.push_chunk(Chunk {
                off: (16 + i) * layout::CHUNK_BYTES,
                borrowed: false,
            });
        }
        st
    }

    #[test]
    fn clock_gives_a_second_chance() {
        // One slot per chunk, so two chunks are exactly the two slots this needs.
        let mut st = store(Class::Huge, 2);
        let r = Register::default();
        let a = st.claim_uncontested(1, r).unwrap();
        st.finish(a, true);
        let b = st.claim_uncontested(2, r).unwrap();
        st.finish(b, true);

        // Touch 1 so it carries a reference bit, then admit a third page.
        assert!(st.find(1).is_some());
        let c = st.claim_uncontested(3, r).unwrap();
        st.finish(c, true);

        assert!(st.find(1).is_some(), "referenced entry must survive");
        assert!(st.find(2).is_none(), "unreferenced entry must be evicted");
        assert!(st.find(3).is_some());
    }

    #[test]
    fn store_never_hands_out_a_busy_slot() {
        let mut st = store(Class::Huge, 1);
        let r = Register::default();
        let i = st.claim_uncontested(1, r).unwrap();
        // The single slot is in flight, so there is nothing to hand out.
        assert_eq!(st.claim_uncontested(2, r), Err(Decline::Busy));
        st.finish(i, false);
        // A failed write leaves no entry behind.
        assert!(st.find(1).is_none());
        assert!(st.claim_uncontested(2, r).is_ok());
    }

    #[test]
    fn empty_store_declines() {
        let mut st = store(Class::Huge, 0);
        assert_eq!(
            st.claim_uncontested(1, Register::default()),
            Err(Decline::Busy)
        );
        assert!(st.find(1).is_none());
    }

    /// A chunk names contiguous media, and consecutive chunks do not overlap.
    #[test]
    fn slots_are_addressed_within_their_chunk() {
        let st = store(Class::Small, 2);
        let cs = chunk_slots(Class::Small) as u32;
        assert_eq!(st.slots.len(), 2 * cs as usize);
        assert_eq!(st.off(0), 16 * layout::CHUNK_BYTES);
        assert_eq!(st.off(1), 16 * layout::CHUNK_BYTES + Class::Small.bytes());
        assert_eq!(
            st.off(cs - 1),
            17 * layout::CHUNK_BYTES - Class::Small.bytes()
        );
        assert_eq!(st.off(cs), 17 * layout::CHUNK_BYTES);
    }

    /// Handing a chunk back drops its own entries and leaves the rest findable at their new
    /// indexes.
    #[test]
    fn giving_a_chunk_back_keeps_the_rest_findable() {
        let mut st = store(Class::Huge, 3);
        let r = Register::default();
        for a in 1..=3u64 {
            let i = st.claim_uncontested(a, r).unwrap();
            st.finish(i, true);
        }
        // Chunk 0 holds address 1. Removing it swaps the last chunk into the hole, so address
        // 3 changes index without changing identity.
        let off = st.remove_chunk(0).unwrap();
        assert_eq!(off, 16 * layout::CHUNK_BYTES);
        assert_eq!(st.chunks.len(), 2);
        assert_eq!(st.slots.len(), 2);
        assert!(st.find(1).is_none(), "the dropped chunk's entry is gone");
        assert_eq!(st.find(2).map(|(i, _)| i), Some(1));
        assert_eq!(st.find(3).map(|(i, _)| i), Some(0), "moved, not lost");
        // Displacement is not demand, so it is counted apart from eviction.
        assert_eq!(st.dropped, 1);
        assert_eq!(st.evicted, 0);
    }

    /// A slot with an IO in flight pins its chunk, and the chunk moved into its place.
    #[test]
    fn a_busy_chunk_is_not_given_back() {
        let mut st = store(Class::Huge, 2);
        let r = Register::default();
        let a = st.claim_uncontested(1, r).unwrap();
        st.finish(a, true);
        // The reference bit sends the hand past slot `a`, so the second claim lands on the
        // other chunk and leaves an IO in flight there.
        assert!(st.find(1).is_some());
        let b = st.claim_uncontested(2, r).unwrap();
        assert_ne!(a, b);
        assert!(st.remove_chunk(b as usize).is_none(), "busy itself");
        let (first, last) = (a.min(b) as usize, a.max(b) as usize);
        assert_eq!(last, st.chunks.len() - 1);
        assert!(
            st.remove_chunk(first).is_none(),
            "the busy chunk would be moved into the hole"
        );
        st.finish(b, true);
        assert!(st.remove_chunk(first).is_some());
    }

    /// `give` answers only with the kind it was asked for: the reclaim path must get a
    /// borrowed chunk and the rebalance must not.
    #[test]
    fn give_returns_only_the_kind_asked_for() {
        let mut st = Store::new(Class::Huge);
        st.push_chunk(Chunk {
            off: 1 << 30,
            borrowed: false,
        });
        st.push_chunk(Chunk {
            off: 2 << 30,
            borrowed: true,
        });
        assert_eq!(st.give(true), Some(2 << 30));
        assert_eq!(st.give(true), None, "nothing borrowed is left");
        assert_eq!(st.give(false), Some(1 << 30));
        assert_eq!(st.give(false), None);
    }

    /// An empty slot is taken without asking, so a cold cache fills at full speed.
    #[test]
    fn an_empty_slot_holds_no_contest() {
        let mut st = store(Class::Huge, 1);
        let i = st.claim(1, Register::default(), |_| true).unwrap();
        st.finish(i, true);
        assert!(st.find(1).is_some());
        assert_eq!(st.evicted, 0);
    }

    /// The contest, and the property the per-extent policy rests on: a resident page in more
    /// demand keeps its slot, so an extent that admits everything cannot scan the rest of the
    /// cache away.
    #[test]
    fn a_hotter_incumbent_keeps_its_slot() {
        let mut st = store(Class::Huge, 1);
        let r = Register::default();
        let i = st.claim_uncontested(1, r).unwrap();
        st.finish(i, true);

        // The incumbent's reference bit is clear (a fresh entry starts that way), so the hand
        // reaches the contest on the first sweep rather than the second.
        assert_eq!(st.claim(2, r, |v| v == 1), Err(Decline::Colder));
        assert!(st.find(1).is_some(), "the incumbent must survive");
        assert!(!st.map.contains_key(&2), "the candidate must not be mapped");
        assert_eq!(st.evicted, 0, "a refused admission is not an eviction");
    }

    /// Ties go to the candidate, so a scan at an estimate of one displaces other pages at one
    /// and nothing above it. Without this a cache of cold entries never turns over.
    #[test]
    fn a_tie_goes_to_the_candidate() {
        let mut st = store(Class::Huge, 1);
        let r = Register::default();
        let i = st.claim_uncontested(1, r).unwrap();
        st.finish(i, true);

        // `hotter` is strict, so equal demand answers false.
        let j = st.claim(2, r, |_| false).unwrap();
        st.finish(j, true);
        assert!(st.find(2).is_some());
        assert!(st.find(1).is_none());
        assert_eq!(st.evicted, 1);
    }

    /// A lost contest ends the admission rather than sweeping on for a weaker victim, so one
    /// refusal costs one slot's work and a hot cache cannot be walked into giving something
    /// up.
    #[test]
    fn a_lost_contest_does_not_keep_sweeping() {
        let mut st = store(Class::Huge, 4);
        let r = Register::default();
        for a in 1..=4 {
            let i = st.claim_uncontested(a, r).unwrap();
            st.finish(i, true);
        }
        // Only the entry the hand happens to reach is consulted, and it wins.
        let hand = st.hand;
        let at = st.slots[hand as usize].addr;
        assert_eq!(st.claim(9, r, |v| v == at), Err(Decline::Colder));
        for a in 1..=4 {
            assert!(st.find(a).is_some(), "entry {a} must survive");
        }
        assert_eq!(st.evicted, 0);
    }

    #[test]
    fn hints_rise_fast_and_fall_slow() {
        assert_eq!(damp(0, 5), 5);
        assert_eq!(damp(5, 9), 9);
        // A drop moves one step, however far the new width is below the old one.
        assert_eq!(damp(9, 0), 8);
        assert_eq!(damp(1, 0), 0);
        assert_eq!(damp(0, 0), 0);
        // Repeated observation of the same lower width walks down, and stops there.
        let mut c = 9;
        for _ in 0..20 {
            c = damp(c, 3);
        }
        assert_eq!(c, 3);
    }

    #[test]
    fn roster_is_this_node_s_cohort_column_in_each_universe() {
        // Member index is the cohort index, so column 1 is ours.
        let cfg = crate::config::Config::parse(
            "node id=5 zone=1 cohort=1 store=/x size=4096
             universe 1
               group 1 5 9
               group 2 5 10
               group 3 7 11
             universe 2
               group 4 5 12",
        )
        .unwrap();
        let r = Roster::of(&cfg);
        assert_eq!(r.me, 5);
        assert_eq!(r.cohort(1), &[5, 7]);
        // A second universe is a second cohort, not more of the first: caching never ranks a
        // peer we share no address space with.
        assert_eq!(r.cohort(2), &[5]);
        // A universe we hold no catalog for is nobody, which reads as caching off.
        assert!(r.cohort(3).is_empty());
        assert_eq!(r.widest(), 2);
    }

    #[test]
    fn a_config_nobody_opts_into_turns_the_cache_off() {
        // A cohort exists, so the roster is structurally able to cache. Whether it should is
        // the extents' call, and here none of them asks to be.
        let text = |admit: &str| {
            format!(
                "node id=5 zone=1 cohort=0 store=/x size=4096
                 universe 1
                   group 5 6 7
                   extent id=1 base=0   pages=16 kind=lww zone=1 {admit}
                   extent id=2 base=16  pages=16 kind=occ zone=1"
            )
        };
        let off = crate::config::Config::parse(&text("")).unwrap();
        let r = Roster::of(&off);
        assert!(
            r.widest() > 0,
            "the cohort is what makes this worth testing"
        );
        assert!(!r.admits);

        // One extent opting in is enough to make the lookup worth its hop.
        let on = crate::config::Config::parse(&text("cache_admit=1")).unwrap();
        assert!(Roster::of(&on).admits);
    }

    #[test]
    fn stripes_cover_every_chunk_exactly_once() {
        for (total, cores) in [(0u64, 4usize), (1, 4), (7, 4), (1024, 3), (100, 7)] {
            let mut next = 0u64;
            for c in 0..cores {
                let (base, len) = stripe(total, cores, c);
                assert_eq!(base, next);
                next += len;
            }
            assert_eq!(next, total);
        }
    }

    /// The cache may only put slots on cores the allocator routes that class to, so a node
    /// with a single shard piles the class onto core 0.
    #[test]
    fn a_single_shard_takes_the_whole_class() {
        let (base, len) = stripe(9, 1, 0);
        assert_eq!((base, len), (0, 9));
    }

    /// A node in a three node zone, holding `small` 4 KiB pages and `huge` 4 MiB ones: three
    /// replicas over three nodes is one share each.
    fn pages(small: u64, huge: u64) -> crate::config::Config {
        let mut s = String::from(
            "node id=1 zone=1 cohort=1 store=/x size=1099511627776
             universe 1
               group 1 2 3\n",
        );
        if small > 0 {
            s.push_str(&format!(
                "  extent id=10 base=0 pages={small} kind=lww zone=1\n"
            ));
        }
        if huge > 0 {
            s.push_str(&format!(
                "  extent id=12 base=1048576 pages={huge} kind=immutable_4m zone=1\n"
            ));
        }
        crate::config::Config::parse(&s).unwrap()
    }

    #[test]
    fn the_split_follows_the_node_s_own_page_mix() {
        // A node holding only 4 MiB pages should not spend the tail on 4 KiB slots.
        let c = pages(0, 1024);
        assert_eq!(c.huge_pages(), 1024);
        assert_eq!(plan_chunks(1000, u64::MAX, &c), [0, 1000]);

        // And the other way round.
        let c = pages(1024, 0);
        assert_eq!(plan_chunks(1000, u64::MAX, &c), [1000, 0]);

        // Bytes, not pages: 1024 small pages are 4 MiB against 1024 huge pages' 4 GiB, so the
        // huge class takes essentially all of it.
        let c = pages(1024, 1024);
        let w = plan_chunks(1000, u64::MAX, &c);
        assert_eq!(w[0] + w[1], 1000);
        assert!(w[1] > w[0] * 100, "got {w:?}");

        // A node hosting neither splits down the middle: a pure cache client has no page mix
        // of its own to go on.
        let c = pages(0, 0);
        assert_eq!(plan_chunks(1000, u64::MAX, &c), [500, 500]);
    }

    /// The DRAM ceiling binds the small class long before the media does, and what small
    /// cannot pay for is not lost.
    #[test]
    fn the_index_budget_caps_the_small_class_first() {
        let c = pages(1024, 0);
        // Room for 4096 slots is four small chunks, however much media there is.
        let w = plan_chunks(1000, 4096, &c);
        assert_eq!(w[0], 4);
        // The rest stays in the pool rather than being forced onto a class the node does not
        // host: the rebalance hands it out when demand shows up.
        assert!(w[0] + w[1] <= 1000);
    }

    #[test]
    fn a_class_that_is_not_evicting_is_not_short() {
        // Nobody evicting, nobody asking: the split stays where it is.
        assert_eq!(wants([100, 100], [0, 0], [0, 0], [1, 1]), None);
        // One class evicting and the other idle needs no comparison at all.
        assert_eq!(wants([1, 1000], [1, 0], [0, 0], [1, 1]), Some(0));
        assert_eq!(wants([1000, 1], [0, 1], [0, 0], [1, 1]), Some(1));
    }

    #[test]
    fn both_short_goes_to_the_better_yield_by_a_margin() {
        let bytes = [1 << 30, 1 << 30];
        let short = [1, 1];
        // Equal yield: neither wins, so nothing moves and the split stops oscillating.
        assert_eq!(wants([100, 100], short, [0, 0], bytes), None);
        // Inside the margin is still noise.
        assert_eq!(wants([150, 100], short, [0, 0], bytes), None);
        // Outside it is a signal.
        assert_eq!(wants([300, 100], short, [0, 0], bytes), Some(0));
        assert_eq!(wants([100, 300], short, [0, 0], bytes), Some(1));
        // Yield is per byte, so half the media at the same hit count wins.
        assert_eq!(
            wants([100, 100], short, [0, 0], [1 << 28, 1 << 30]),
            Some(0)
        );
    }

    /// A class the plan gave nothing can never evict, so eviction alone would starve it
    /// forever; a miss against an empty class is the signal instead.
    #[test]
    fn a_class_holding_nothing_asks_with_misses() {
        assert_eq!(wants([0, 500], [0, 0], [7, 0], [0, 1 << 30]), Some(0));
        // But only if something is actually asking for it.
        assert_eq!(wants([0, 500], [0, 0], [0, 0], [0, 1 << 30]), None);
        // An empty class beats a measured one outright: one chunk is the cost of finding out
        // whether it deserves more.
        assert_eq!(wants([0, 500], [0, 1], [7, 0], [0, 1 << 30]), Some(0));
    }
}
