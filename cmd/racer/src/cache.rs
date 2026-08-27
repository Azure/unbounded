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
use std::rc::Rc;
use std::time::{Duration, Instant};

mod rebalance;
mod store;

use crate::alloc::{Allocator, GlobalAddr, Pressure};
use crate::config::{self, Config, rank};
use crate::layout::{self, Class};
use crate::paxos::Register;
use crate::runtime::{self, Buf, CoreId, Disk, Durability};
use crate::server::{Server, Worker};

#[cfg(test)]
use rebalance::wants;
use rebalance::{OPEN_SHARE, Pool, REBALANCE, plan_chunks};
use store::{Admit, Chunk, Decline, Lease, Store, chunk_slots};

// --- tunables ---
//
// Not configuration. Sketch geometry, decay interval and width cap are correctness-
// independent, so a node picks them without the control plane knowing anything.

/// Count-min rows. Four is where the collision rate stops mattering next to the 4-bit
/// saturation.
const ROWS: usize = 4;
// Counters per row when this core holds no slots (the floor everywhere), and at the
// ceiling, both powers of two so the index is a mask. At the floor the packed nibbles are
// 128 KiB, which is L2-resident. The sketch must scale with the cache, or a store whose
// tail holds millions of slots filters admissions off a sketch sized for tens of thousands
// and every cold page looks hot; the cap keeps it L3-resident. Both come from the
// runtime's limits, so a compact node gets a table it can afford.
/// A nibble saturates here.
const COUNTER_MAX: u8 = 15;
/// Halvings that take a saturated counter to zero: `15 -> 7 -> 3 -> 1 -> 0`.
const ZEROED_AFTER: u32 = COUNTER_MAX.ilog2() + 1;
/// Halving at this interval turns the sketch into an exponentially weighted rate.
const DECAY: Duration = Duration::from_millis(250);
/// `W_max` before the cohort size caps it.
const W_CAP: u8 = 64;
// Reader-side width hints, direct mapped, sized like the sketch and for the same reason. A
// collision mis-hints one address, costing one `MISSING` and a fallback, so there is
// nothing to resolve.

/// DRAM a resident cache slot costs: the [`store::Slot`] record plus its share of the map.
/// `policy.cache_index_bytes` divided by this bounds slots per node, which stops a large tail
/// from turning into an OOM.
pub const BYTES_PER_SLOT: u64 = 48;

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
        let c = cfg.node.cohort() as usize;
        let cohorts = cfg
            .universes()
            .iter()
            .map(|u| {
                let mut nodes: Vec<u32> = u.catalog.iter().filter_map(|g| g.cohort(c)).collect();
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

// --- per-core state ---

/// Everything the cache keeps on one core.
///
/// Two shardings meet here. The sketch and the stores are reached on the core owning the
/// address's consensus group, so they ride the hop the round already takes and the sketch sees
/// the whole read stream. The hints are reached on whatever core handles the request, where
/// the reply trailer carrying the width lands; a hint is advisory, so per-core is fine.
pub(crate) struct Local {
    sketch: Sketch,
    store: Store,
    hints: Box<[u8]>,
    /// `hints.len() - 1`.
    hint_mask: usize,
    decayed: Instant,
    stats: Stats,
}

impl Local {
    /// Bump counters on the share already open. Taking the share rather than reaching for it
    /// is what lets a transaction count what it just did.
    fn stat(&mut self, f: impl FnOnce(&mut Stats)) {
        f(&mut self.stats);
    }
}

/// Per-core counters, read by [`Cache::local_stats`]; the exporter sums cores.
#[derive(Clone, Copy, Default)]
pub struct Stats {
    pub hits: u64,
    pub misses: u64,
    /// Hits that then passed confirmation against the quorum. The gap against `hits` is what
    /// staleness costs.
    pub served: u64,
    pub admits: u64,
    pub evictions: u64,
    /// Entries lost because their chunk was handed back to the free tail, as opposed to
    /// evicted to make room for a hotter block.
    pub dropped: u64,
    pub stale: u64,
    pub shed: u64,
    /// Admissions refused because the block is mutable, or because the extent's
    /// `cache_admit` said no: the extent caches nothing, or the block had not shown the demand
    /// it asks for. Counted where the decision is made, which is the group member computing
    /// the width rather than the node that would have cached. A client seeing few admits and
    /// no rejections here is being vetoed elsewhere.
    pub rejected_policy: u64,
    /// Admissions refused because the entry at the clock hand was in more demand. Rising with
    /// a full cache is the contest doing its job; rising with a cache that is not full means
    /// the cache is short of slots, not short of demand.
    pub rejected_victim: u64,
    /// Media the cache holds on this core.
    pub bytes: u64,
}

// --- Cache ---

pub struct Cache {
    alloc: &'static Allocator,
    disk: Disk,
    /// The tail this cache was handed at open: base and chunk count. Fixed for the life of the
    /// process, because the layout it comes from is.
    tail: (u64, u64),
    /// Cores that can own a cacheable address, so how many may hold slots.
    shards: usize,
    /// Slots the node may hold across all cores, from `policy.cache_index_bytes`.
    budget: u64,
}

/// One worker's share of the cache, living in that worker's [`crate::server::CoreState`].
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
    let (base, _) = geo.tail(cfg.node.store_bytes());
    let chunks = geo.tail_chunks(cfg.node.store_bytes());
    let budget = cfg.cache_index_bytes() / BYTES_PER_SLOT;
    // Only cores a cacheable address can hop to. `owner_core` routes every lookup through
    // `shards_for`, so slots placed on a core past that are unreachable. Everything cacheable
    // is immutable, so that is the only slab that matters here.
    let shards = alloc.shards_for(Class::Immutable).min(cores).max(1);

    let want = plan_chunks(chunks, budget / OPEN_SHARE);
    let now = crate::runtime::now();
    // Sized from what a core may end up holding, not from what it starts with: both tables are
    // approximations whose error is what they cost, and resizing either would throw away the
    // history that makes them worth keeping.
    let share = (budget / cores as u64).max(1);
    let limits = crate::runtime::limits();
    let (cols, hints) = (
        table_len(share, limits.cache_cols_min, limits.cache_cols_max),
        table_len(share, limits.cache_hints_min, limits.cache_hints_max),
    );

    let state: Vec<Local> = (0..cores)
        .map(|c| {
            let mut store = Store::new();
            if c < shards {
                let (lo, len) = stripe(want, shards, c);
                for i in 0..len {
                    store.push_chunk(Chunk {
                        off: base + (lo + i) * layout::CHUNK_BYTES,
                    });
                }
            }
            Local {
                sketch: Sketch::new(cols),
                hints: vec![0u8; hints].into_boxed_slice(),
                hint_mask: hints - 1,
                store,
                decayed: now + phase(c, cores),
                stats: Stats::default(),
            }
        })
        .collect();

    let mut free = Some(
        (want..chunks)
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
fn here<T>(worker: &Worker, f: impl FnOnce(&mut Local) -> T) -> T {
    f(&mut worker.core().cache.local.borrow_mut())
}

/// `core`'s share of the cache, as a transaction that core runs.
///
/// The whole body is synchronous, which is the point: a lookup, a claim or a hand-back is one
/// visit to the owning worker rather than a task parked on it.
fn at<T, F>(core: CoreId, f: F) -> impl Future<Output = T>
where
    F: FnOnce(&Worker, &mut Local) -> T + Send + 'static,
    T: Send + 'static,
{
    runtime::to::<Server, _, _>(core, move |_, worker| {
        f(worker, &mut worker.core().cache.local.borrow_mut())
    })
}

/// The free tail, on the one core that has it; `None` anywhere else.
fn pool<T>(worker: &Worker, f: impl FnOnce(&mut Pool) -> T) -> Option<T> {
    worker
        .core()
        .cache
        .pool
        .as_ref()
        .map(|p| f(&mut p.borrow_mut()))
}

impl Cache {
    /// The core owning `addr` for its class, named rather than indexed.
    fn owner_of(&self, worker: &Worker, addr: GlobalAddr) -> Option<CoreId> {
        let c = self.alloc.owner_core(worker, addr).ok()?;
        Some(c)
    }

    /// Whether this node can cache at all. Structural only, and deliberately cheap: a cohort
    /// of nobody leaves no peer to place a replica on, and a config where no extent opts in
    /// leaves nothing to place. Both are properties of the config rather than the address, so
    /// a lookup can be refused before it costs a hop. Whether a particular page should be
    /// cached is per extent and lives in `observe_local`.
    fn enabled(&self, cfg: &config::Compiled) -> bool {
        let r = cfg.roster();
        r.admits && r.widest() > 0
    }

    /// Sheds while the allocator is short of free space, or while the store's rate budget is
    /// committed ahead. Cache space is statically separate from the allocator's but store
    /// bandwidth is not, so the cache stops admitting before anything authoritative slows
    /// down. An inbound extent migration reaches the cache through this alone.
    pub fn shedding(&self, worker: &Worker) -> bool {
        self.alloc.pressure(worker) != Pressure::Normal || self.alloc.store_pressed()
    }

    fn stat(&self, worker: &Worker, f: impl FnOnce(&mut Stats)) {
        here(worker, |l| l.stat(f));
    }

    /// This core's counters. Evictions and capacity are the store's own state rather than
    /// separate counters.
    pub fn local_stats(&self, worker: &Worker) -> Stats {
        here(worker, |l| {
            let mut s = l.stats;
            s.evictions = l.store.evicted;
            s.dropped = l.store.dropped;
            s.bytes = l.store.chunks.len() as u64 * layout::CHUNK_BYTES;
            s
        })
    }

    /// Count a cached block that passed confirmation. The one counter the cache cannot keep
    /// itself, because confirmation happens inside the consensus round.
    pub fn served(&self, worker: &Worker) {
        self.stat(worker, |s| s.served += 1);
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
    pub fn observe_local(&self, worker: &Worker, addr: GlobalAddr) -> u8 {
        here(worker, |l| self.observe_in(worker.compiled(), l, addr))
    }

    /// [`Self::observe_local`] against a share already open, so the counters it bumps and the
    /// sketch it feeds are the same borrow.
    fn observe_in(&self, cfg: &config::Compiled, l: &mut Local, addr: GlobalAddr) -> u8 {
        // Nothing to advertise a width to, so nothing to count either: a node whose cache is
        // off by config would otherwise report every read as a rejection.
        if !self.enabled(cfg) {
            return 0;
        }
        // One lookup for both the kind and the threshold; an address in no extent of ours is
        // not a rejection, it is nothing to reject.
        let Some(e) = cfg.config().extent_at(addr.0) else {
            return 0;
        };
        // Only immutable blocks are cacheable. A mutable block's value changes under a
        // register the cache cannot follow, and it is checksummed against its own version, so
        // there is nothing here for it: refuse before the sketch ever sees it.
        if e.guard() != config::Kind::Immutable {
            l.stat(|s| s.rejected_policy += 1);
            return 0;
        }
        let n = e.cache_admit as u8;
        if n == 0 {
            l.stat(|s| s.rejected_policy += 1);
            return 0;
        }
        let cap = self.w_max(cfg.roster());
        // Observe before testing, so a page below the threshold still accumulates the demand
        // that would carry it over. `n == 1` therefore always passes: the sketch never answers
        // below one for a key it has just seen.
        let q = l.sketch.observe(addr.0);
        if q < n {
            l.stat(|s| s.rejected_policy += 1);
            return 0;
        }
        q.min(cap)
    }

    /// `W_max = min(cohort_size, 64)`.
    fn w_max(&self, roster: &Roster) -> u8 {
        u8::try_from(roster.widest()).unwrap_or(W_CAP).min(W_CAP)
    }

    // --- hints ---

    /// The width last advertised for `addr`, as this core remembers it. The cached leg and the
    /// metadata round are issued together, but `w` arrives *in* that round's reply, so the leg
    /// can only be taken on a width learned from an earlier read. The first read of a key is
    /// therefore always uncached, which is what the admission filter wants.
    pub fn hint(&self, worker: &Worker, addr: GlobalAddr) -> u8 {
        here(worker, |l| {
            l.hints[config::mix(addr.0) as usize & l.hint_mask]
        })
    }

    /// Record the width from a reply trailer, damped.
    pub fn note_hint(&self, worker: &Worker, addr: GlobalAddr, w: u8) {
        here(worker, |l| {
            let i = config::mix(addr.0) as usize & l.hint_mask;
            l.hints[i] = damp(l.hints[i], w);
        });
    }

    // --- placement ---

    /// Whether we are one of the `w` replicas for `addr`: the number of cohort peers that
    /// outrank us is below the width.
    pub fn holds(&self, worker: &Worker, addr: GlobalAddr, w: u8) -> bool {
        let r = worker.compiled().roster();
        let nodes = r.cohort(addr.universe());
        if w == 0 || nodes.is_empty() {
            return false;
        }
        let mine = rank(addr.0, r.me);
        nodes.iter().filter(|&&n| rank(addr.0, n) > mine).count() < w as usize
    }

    /// The highest-ranked live member of `R`, excluding ourselves. `ok` is the reachability
    /// test: a cohort peer we hold no link to is not a candidate.
    pub fn replica(
        &self,
        worker: &Worker,
        addr: GlobalAddr,
        w: u8,
        ok: impl Fn(u32) -> bool,
    ) -> Option<u32> {
        let r = worker.compiled().roster();
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

    /// Read `addr` out of the local cache region into `buf`, returning the register the entry
    /// claims. The caller confirms that register against the quorum; this makes no claim about
    /// freshness.
    pub async fn load(&'static self, worker: &Worker, addr: GlobalAddr, buf: Buf) -> Option<Register> {
        self.load_at(worker, addr, buf, None).await
    }

    /// The warmed path. An entry is servable only while it carries the current epoch's live
    /// version, so an epoch bump invalidates every entry with one comparison; that is the
    /// whole invalidation protocol.
    pub async fn load_immutable(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        buf: Buf,
    ) -> Option<Register> {
        if !self.enabled(worker.compiled()) {
            return None;
        }
        self.load_at(
            worker,
            addr,
            buf,
            Some(self.live_version(worker.config(), addr)),
        )
        .await
    }

    /// What our cached copy of `addr` claims, with no bytes moved. Filtered exactly as
    /// [`Self::load_immutable`] filters, so the two agree on which entry they mean.
    pub async fn peek_immutable(&'static self, worker: &Worker, addr: GlobalAddr) -> Option<Register> {
        if !self.enabled(worker.compiled()) {
            return None;
        }
        let want = self.live_version(worker.config(), addr);
        let owner = self.owner_of(worker, addr)?;
        at(owner, move |_, l| {
            // No bytes move, so the pin `find_in` takes has nothing to protect: let it go
            // before the transaction ends rather than handing a lease across the hop.
            let (slot, _, reg) = self.find_in(l, addr, Some(want))?;
            l.store.release(slot);
            Some(reg)
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
        worker: &Worker,
        addr: GlobalAddr,
        buf: Buf,
        want: Option<u64>,
    ) -> Option<Register> {
        let owner = self.owner_of(worker, addr)?;
        let (slot, found, reg) = at(owner, move |_, l| self.find_in(l, addr, want)).await?;
        // Built here, from what the transaction answered with, for the same reason the
        // admission permit is: a value that crosses a hop can be dropped inside the
        // rendezvous, which is no place for a destructor that wants to hop of its own.
        let lease = Lease {
            cache: self,
            owner,
            slot,
        };
        if buf.len() as u64 != layout::SMALL_PAGE {
            return None;
        }
        let read = self.disk.read(found, buf).await;
        lease.release().await;
        if read.is_err() {
            // A cache page we cannot read is one we do not have. Silent rot is invisible here
            // (confirmation covers the register, not the bytes), but a hard read error means
            // the entry is gone. At `reg`, because the read took a round trip and what is
            // there now may be a newer value that reads perfectly well.
            at(owner, move |_, l| {
                l.store.forget_at(addr.0, reg);
                l.stat(|s| s.misses += 1);
            })
            .await;
            return None;
        }
        self.stat(worker, |s| s.hits += 1);
        Some(reg)
    }

    /// Find `addr`'s entry and pin its media, on the owning core because only that core knows
    /// which chunks its store holds.
    ///
    /// Answers with the offset as well as the slot, because the caller does the IO and an
    /// offset is what a disk takes. The slot comes too so the pin can be let go of, and the
    /// pin is what keeps the two agreeing: while it is held the chunk cannot be handed away,
    /// so neither the offset nor the index can come to mean something else.
    fn find_in(
        &self,
        l: &mut Local,
        addr: GlobalAddr,
        want: Option<u64>,
    ) -> Option<(u32, u64, Register)> {
        let found = l.store.find(addr.0);
        let Some((slot, reg)) = found.filter(|v| want.is_none_or(|w| v.1.version == w)) else {
            l.stat(|s| s.misses += 1);
            return None;
        };
        // A hit is a read too, and this is the only place this node learns of one: group
        // members see the metadata round, but a hit served from here never reaches them, so a
        // resident entry's estimate would only decay and lose the contest to the first
        // candidate walking past.
        l.sketch.observe(addr.0);
        Some((slot, l.store.off(slot), reg))
    }

    /// Offer `buf` to the cache as the value of `addr` at `reg`, given the width `w` its owner
    /// last advertised. Declines silently: not one of the `w` replicas, not hot enough, device
    /// under pressure, or every slot being written. The cache never fails a write, it declines
    /// one.
    pub async fn admit(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        buf: Buf,
        reg: Register,
        w: u8,
    ) {
        if !self.enabled(worker.compiled()) || !self.holds(worker, addr, w) {
            return;
        }
        let Some(owner) = self.owner_of(worker, addr) else {
            return;
        };
        // Claim on the owning core, write here, then report back: the buffer's registration
        // belongs to this core's ring and cannot travel (see `load_at`).
        let Some((slot, dst)) = at(owner, move |worker, l| {
            self.claim_in(worker, worker.config(), l, addr, reg)
        })
        .await
        else {
            return;
        };
        // Built here, from a plain pair, rather than handed back by the transaction: a value
        // that crosses a hop can be dropped inside the rendezvous, which is no place for a
        // destructor that wants to hop of its own.
        let permit = Admit {
            cache: self,
            owner,
            slot,
        };
        // One IO, in place, `Buffered`: no torn-write hazard, because nothing points at these
        // bytes after a restart. The slot stays busy across it, which also stops its chunk
        // from being handed away underneath the write.
        let ok = buf.len() as u64 == layout::SMALL_PAGE
            && self
                .disk
                .write(dst, buf, Durability::Buffered)
                .await
                .is_ok();
        permit.settle(ok).await;
    }

    /// Let go of a slot a read was pinning. Not called directly: [`Lease`] is the only holder.
    async fn unpin(&'static self, owner: CoreId, slot: u32) {
        at(owner, move |_, l| l.store.release(slot)).await;
    }

    /// Report an admission back to the core that claimed the slot. Not called directly:
    /// [`Admit`] is the only holder.
    async fn report(&'static self, owner: CoreId, slot: u32, ok: bool) {
        at(owner, move |_, l| {
            l.store.finish(slot, ok);
            if ok {
                l.stat(|s| s.admits += 1);
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
        worker: &Worker,
        cfg: &Config,
        l: &mut Local,
        addr: GlobalAddr,
        reg: Register,
    ) -> Option<(u32, u64)> {
        if l.store.current(addr.0, reg) {
            return None;
        }
        if self.shedding(worker) {
            l.stat(|s| s.shed += 1);
            return None;
        }
        // Mutable blocks are never cached, and the kill switch has to bite the moment the
        // config lands rather than after the reader's damped width hint drains.
        if !cfg
            .extent_at(addr.0)
            .is_some_and(|e| e.guard() == config::Kind::Immutable && e.cache_admit != 0)
        {
            l.stat(|s| s.rejected_policy += 1);
            return None;
        }
        // Count the read this node is about to serve from its own cache: `observe_local` runs
        // on a group member and this node is by construction not one, so otherwise the local
        // sketch answers zero for every block and the contest below is a coin toss it always
        // wins.
        let cand = l.sketch.observe(addr.0);
        let (sketch, store) = (&l.sketch, &mut l.store);
        // The contest, and the only place hotness reaches victim selection. Ties go to the
        // candidate, so a cold scan at an estimate of one churns other cold entries and leaves
        // anything at two or more alone.
        let slot = match store.claim(addr.0, reg, |victim| sketch.estimate(victim) > cand) {
            Ok(slot) => slot,
            Err(Decline::Colder) => {
                l.stats.rejected_victim += 1;
                return None;
            }
            Err(Decline::Busy) => return None,
        };
        Some((slot, l.store.off(slot)))
    }

    /// Drop whatever is cached for `addr`, whatever it is. For a caller that knows the
    /// address itself is gone, rather than one that found a particular value stale.
    pub async fn forget(&'static self, worker: &Worker, addr: GlobalAddr) {
        let Some(owner) = self.owner_of(worker, addr) else {
            return;
        };
        at(owner, move |_, l| {
            l.store.forget(addr.0);
            l.stat(|s| s.stale += 1);
        })
        .await;
    }

    /// Drop `addr`'s entry if it is still the one at `reg`.
    ///
    /// What a failed confirmation actually justifies. The register is the reader's evidence,
    /// and it is about the value the reader saw: taking it here rather than throwing it away
    /// keeps a late rejection from evicting the newer entry that replaced it.
    pub async fn forget_stale(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        reg: Register,
    ) {
        let Some(owner) = self.owner_of(worker, addr) else {
            return;
        };
        at(owner, move |_, l| {
            l.store.forget_at(addr.0, reg);
            l.stat(|s| s.stale += 1);
        })
        .await;
    }

    // --- immutable ---

    /// The version an immutable block holds while it is live at its extent's epoch. Only
    /// quorum-confirmed values are admitted, and one version has exactly one such value, so a
    /// version is a complete identity for a cached block.
    fn live_version(&self, cfg: &Config, addr: GlobalAddr) -> u64 {
        3 * cfg.tombstone_epoch_of(addr.0) + 1
    }

    // --- tick ---

    /// The decay, and where the pool is the rebalance. Driven from `Handler::tick`, which is a
    /// poll and not a timer: an idle worker takes no ticks, so the halving count comes from
    /// elapsed time rather than assumed to be one.
    pub fn tick(&'static self, worker: Rc<Worker>, now: Instant) {
        here(&worker, |l| {
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
        let start = pool(&worker, |p| {
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
        let detached = worker.clone();
        if !runtime::spawn_local(async move {
            self.rebalance(detached.clone()).await;
            pool(&detached, |p| p.running = false);
        }) {
            pool(&worker, |p| p.running = false);
        }
    }
}

// --- tests ---

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sketch_is_one_sided_and_decays() {
        let mut s = Sketch::new(crate::runtime::limits().cache_cols_min);
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
        let mut s = Sketch::new(crate::runtime::limits().cache_cols_min);
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
            let (mut fast, mut slow) = (
                Sketch::new(crate::runtime::limits().cache_cols_min),
                Sketch::new(crate::runtime::limits().cache_cols_min),
            );
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
        let mut s = Sketch::new(crate::runtime::limits().cache_cols_min);
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
        let mut s = Sketch::new(crate::runtime::limits().cache_cols_min);
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

    /// A store holding `n` slots, cut from as many chunks of tail as that takes. Trimming the
    /// tail of the last chunk keeps the replacement tests small enough to reason about; the
    /// addressing tests below use whole chunks.
    fn store(n: usize) -> Store {
        let cs = chunk_slots();
        let mut st = Store::new();
        for i in 0..n.div_ceil(cs).max(1) as u64 {
            st.push_chunk(Chunk {
                off: (16 + i) * layout::CHUNK_BYTES,
            });
        }
        st.slots.truncate(n);
        st
    }

    #[test]
    fn clock_gives_a_second_chance() {
        // One slot per chunk, so two chunks are exactly the two slots this needs.
        let mut st = store(2);
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
        let mut st = store(1);
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
        let mut st = store(0);
        assert_eq!(
            st.claim_uncontested(1, Register::default()),
            Err(Decline::Busy)
        );
        assert!(st.find(1).is_none());
    }

    /// A chunk names contiguous media, and consecutive chunks do not overlap.
    #[test]
    fn slots_are_addressed_within_their_chunk() {
        let cs = chunk_slots() as u32;
        let st = store(2 * cs as usize);
        assert_eq!(st.slots.len(), 2 * cs as usize);
        assert_eq!(st.off(0), 16 * layout::CHUNK_BYTES);
        assert_eq!(st.off(1), 16 * layout::CHUNK_BYTES + layout::SMALL_PAGE);
        assert_eq!(
            st.off(cs - 1),
            17 * layout::CHUNK_BYTES - layout::SMALL_PAGE
        );
        assert_eq!(st.off(cs), 17 * layout::CHUNK_BYTES);
    }

    /// A read in flight pins its media exactly as a write does: the entry cannot be evicted,
    /// the slot cannot be re-admitted, and the chunk cannot be handed away.
    #[test]
    fn a_read_in_flight_pins_its_slot() {
        let mut st = store(1);
        let r = Register::default();
        let a = st.claim_uncontested(1, r).unwrap();
        st.finish(a, true);
        assert!(st.find(1).is_some());

        // The only slot is pinned, so the hand comes back around with nowhere to go.
        assert_eq!(st.claim(2, r, |_| false), Err(Decline::Busy));
        // Not even for the address being read: the write would land under the read.
        assert_eq!(st.claim(1, r, |_| false), Err(Decline::Busy));

        st.release(a);
        assert!(st.claim_uncontested(2, r).is_ok());
    }

    /// Two readers of one entry, and the media stays put until the second is done.
    #[test]
    fn pins_are_counted_not_flagged() {
        let mut st = store(1);
        let r = Register::default();
        let a = st.claim_uncontested(1, r).unwrap();
        st.finish(a, true);
        assert!(st.find(1).is_some());
        assert!(st.find(1).is_some());

        st.release(a);
        assert_eq!(
            st.claim(2, r, |_| false),
            Err(Decline::Busy),
            "one reader is still out"
        );
        st.release(a);
        assert!(st.claim_uncontested(2, r).is_ok());
    }

    /// A rejection is about the entry that was read. By the time it comes back a newer one
    /// may hold the slot, and that one is what the next reader wants.
    #[test]
    fn forgetting_stale_spares_a_newer_entry() {
        let mut st = store(1);
        let old = Register {
            version: 1,
            ..Register::default()
        };
        let new = Register {
            version: 2,
            ..Register::default()
        };
        let i = st.claim_uncontested(1, old).unwrap();
        st.finish(i, true);

        // The reader is still holding `old` when the entry is replaced.
        let j = st.claim_uncontested(1, new).unwrap();
        st.finish(j, true);

        st.forget_at(1, old);
        assert!(
            st.current(1, new),
            "the newer entry survives a late rejection"
        );
        st.forget_at(1, new);
        assert!(!st.current(1, new));
    }

    /// An empty slot is taken without asking, so a cold cache fills at full speed.
    #[test]
    fn an_empty_slot_holds_no_contest() {
        let mut st = store(1);
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
        let mut st = store(1);
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
        let mut st = store(1);
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
        let mut st = store(4);
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
                   extent id=1 base=0    blocks=16 kind=immutable zone=1 {admit}
                   extent id=2 base=1024 blocks=16 kind=immutable zone=1"
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

    /// Only immutable extents may ask to be cached, so a mutable one never opts in however
    /// its policy reads.
    #[test]
    fn a_mutable_extent_cannot_turn_the_cache_on() {
        let cfg = crate::config::Config::parse(
            "node id=5 zone=1 cohort=0 store=/x size=4096
             universe 1
               group 5 6 7
               extent id=1 base=0 blocks=16 kind=mutable zone=1 cache_admit=1",
        )
        .unwrap();
        let r = Roster::of(&cfg);
        assert!(r.widest() > 0, "the cohort is what makes this worth testing");
        assert!(!r.admits, "a mutable extent may not be cached");
    }

    /// The tail is one class now, so the plan is media against the DRAM ceiling and nothing
    /// else. Whatever the budget cannot index stays in the pool for the rebalance to hand out.
    #[test]
    fn the_index_budget_caps_the_plan() {
        assert_eq!(plan_chunks(1000, u64::MAX), 1000);
        // Room for 4096 slots is four chunks, however much media there is.
        assert_eq!(plan_chunks(1000, 4096 * BYTES_PER_SLOT), 4);
        assert_eq!(plan_chunks(1000, 0), 0);
    }

    /// Growth is demand driven: a store that is not evicting has nothing to gain from more
    /// media, and one holding nothing at all asks with misses instead.
    #[test]
    fn growth_follows_eviction_or_an_empty_store() {
        assert!(!wants(0, 0, 0));
        assert!(wants(1, 0, 1 << 30));
        assert!(wants(0, 7, 0), "empty and asked for");
        assert!(!wants(0, 0, 0));
        assert!(!wants(0, 7, 1 << 30), "misses alone are not pressure");
    }
}
