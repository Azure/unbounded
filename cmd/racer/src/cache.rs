// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Cooperative caching: which nodes keep a copy of which page, so reads of a hot page
//! do not all land on the three that own it.
//!
//! No consistency of its own. Every cached read is confirmed by the mandatory metadata
//! round, so staleness is self-correcting and there is no invalidation protocol. Never
//! a correctness dependency: every entry point may decline, at the cost of one ordinary
//! read.
//!
//! Three properties keep this file small:
//!
//!   * **No directory.** Placement is a rendezvous hash over the reader's own cohort,
//!     so any reader computes the replica set locally.
//!   * **No per-key state.** Hotness is a count-min sketch, width is arithmetic on it,
//!     and the reader's width hint is a direct-mapped byte array. Nothing grows with
//!     the key space and nothing allocates after `open`.
//!   * **No durability.** The map is volatile, writes are in place and `Buffered`, and
//!     the region is carved at format time. A torn cache page is unreachable after a
//!     restart because nothing points at it, so the allocator's out-of-place discipline
//!     does not apply here.
//!
//! Choices worth naming:
//!
//!   * The rendezvous hash is [`config::mix`]. It is never on the wire; only local
//!     computability and `R(k,w) ⊂ R(k,w+1)` matter.
//!   * A shedding replica answers `MISSING` rather than a busy status of its own, since
//!     only four errnos survive nvmet. The reader's fallback is identical either way.
//!   * `τ` is requests per decay interval, not an IOPS rate: it is clamped to the
//!     sketch's ceiling of 15 per interval, so an IOPS-scale value yields `w ≤ 1`.

use std::cell::RefCell;
use std::collections::HashMap;
use std::time::{Duration, Instant};

use crate::alloc::{Allocator, GlobalAddr, Pressure};
use crate::config::{self, Config};
use crate::layout::{Class, Geometry};
use crate::paxos::Register;
use crate::runtime::{self, Buf, Disk, Durability};

// ---------------------------------------------------------------------------
// tunables
// ---------------------------------------------------------------------------
//
// Not configuration. Sketch geometry, decay interval and width cap are deliberately
// absent from the config file: they are correctness-independent, so a node picks them
// without the control plane knowing anything.

/// Count-min rows. Four is where the collision rate stops mattering next to the 4-bit
/// saturation.
const ROWS: usize = 4;
/// Counters per row. A power of two so the index is a mask; as packed nibbles the
/// whole sketch is `ROWS * COLS / 2` = 128 KiB, sized once and L2-resident.
#[cfg(not(feature = "sim"))]
const COLS: usize = 1 << 16;
#[cfg(feature = "sim")]
const COLS: usize = 1 << 10;
/// A nibble saturates here.
const COUNTER_MAX: u8 = 15;
/// Halving at this interval turns the sketch into an exponentially weighted rate.
const DECAY: Duration = Duration::from_millis(250);
/// `W_max` before the cohort size caps it.
const W_CAP: u8 = 64;
/// Reader-side width hints, direct mapped. A collision mis-hints one address, costing
/// one `MISSING` and a fallback, so there is nothing to resolve.
#[cfg(not(feature = "sim"))]
const HINTS: usize = 1 << 16;
#[cfg(feature = "sim")]
const HINTS: usize = 1 << 10;

/// Per-row salt, so the rows are independent hashes of the same address.
const SALT: [u64; ROWS] = [
    0x9e37_79b9_7f4a_7c15,
    0xc2b2_ae3d_27d4_eb4f,
    0x1656_67b1_9e37_79f9,
    0xff51_afd7_ed55_8ccd,
];

// ---------------------------------------------------------------------------
// sketch
// ---------------------------------------------------------------------------

/// Count-min with conservative update and periodic halving.
///
/// Conservative update makes the estimator one-sided: it may over-count but never
/// under-counts. An over-estimate only over-replicates a page, which `W_max` bounds and
/// the next decay corrects.
struct Sketch {
    /// One packed nibble array per row.
    rows: [Box<[u8]>; ROWS],
}

impl Sketch {
    fn new() -> Sketch {
        Sketch {
            rows: std::array::from_fn(|_| vec![0u8; COLS / 2].into_boxed_slice()),
        }
    }

    fn cell(row: usize, addr: u64) -> usize {
        config::mix(addr ^ SALT[row]) as usize & (COLS - 1)
    }

    fn get(&self, row: usize, i: usize) -> u8 {
        (self.rows[row][i >> 1] >> ((i & 1) * 4)) & 0xf
    }

    fn set(&mut self, row: usize, i: usize, v: u8) {
        let b = &mut self.rows[row][i >> 1];
        let sh = (i & 1) * 4;
        *b = (*b & !(0xf << sh)) | (v << sh);
    }

    /// The rate estimate `q̂(k)`, in counts since the last halving.
    fn estimate(&self, addr: u64) -> u8 {
        (0..ROWS)
            .map(|r| self.get(r, Sketch::cell(r, addr)))
            .min()
            .unwrap_or(0)
    }

    /// Record one request and return the new estimate. Only rows sitting at the current
    /// minimum are raised, which is the whole of "conservative".
    fn observe(&mut self, addr: u64) -> u8 {
        let cells: [usize; ROWS] = std::array::from_fn(|r| Sketch::cell(r, addr));
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

    /// Halve every counter `n` times. Masking with `0x77` after each shift drops the
    /// bit that would otherwise fall out of the high nibble into the low one.
    fn halve(&mut self, n: u32) {
        if n == 0 {
            return;
        }
        for row in &mut self.rows {
            if n >= 4 {
                row.fill(0);
            } else {
                for b in row.iter_mut() {
                    for _ in 0..n {
                        *b = (*b >> 1) & 0x77;
                    }
                }
            }
        }
    }
}

// ---------------------------------------------------------------------------
// placement
// ---------------------------------------------------------------------------

/// This node's cohort, as the config implies it.
///
/// There is no cohort roster in the schema. `Node.cohort` names a catalog column, so a
/// cohort is the projection of `topology.catalog` onto that column. Every node claiming
/// column `c` sees the same catalog and derives the same roster, which is what local
/// computation of `R` requires. The schema does not check that a node actually occupies
/// its column in every group — a catalog may permute members to spread the paxos member
/// index — so the cohort is the label the control plane assigns, not one derived here.
#[derive(Default)]
struct Roster {
    me: u32,
    nodes: Box<[u32]>,
}

impl Roster {
    fn of(cfg: &Config) -> Roster {
        let c = cfg.node.cohort as usize;
        let mut nodes: Vec<u32> = cfg.topology.catalog.iter().map(|g| g[c]).collect();
        nodes.push(cfg.node.id);
        nodes.sort_unstable();
        nodes.dedup();
        Roster {
            me: cfg.node.id,
            nodes: nodes.into_boxed_slice(),
        }
    }
}

/// The rendezvous score. Nesting — `R(k,w) ⊂ R(k,w+1)` — is automatic: the ranking is a
/// total order independent of `w`, so raising the width only appends.
fn rank(addr: u64, node: u32) -> u64 {
    config::mix(addr ^ config::mix(node as u64))
}

// ---------------------------------------------------------------------------
// store
// ---------------------------------------------------------------------------

/// One cache slot. Subtractive next to an allocator entry: no CRC, no generation, no
/// A/B copy, no group commit. The register is here only so a hit can be confirmed
/// against the quorum; nothing here survives a restart.
#[derive(Clone, Copy, Default)]
struct Slot {
    addr: u64,
    reg: Register,
    /// CLOCK reference bit.
    used: bool,
    live: bool,
    /// A write is in flight into this slot. CLOCK steps over it rather than handing
    /// the same media out twice.
    busy: bool,
}

/// This core's stripe of one class's cache region.
struct Store {
    class: Class,
    /// First device slot of the stripe. Slot `i` here is device slot `base + i`.
    base: u32,
    slots: Box<[Slot]>,
    /// `GlobalAddr -> local slot`. Sized once; entries can never exceed `slots`, so it
    /// never grows and never allocates on the hot path.
    map: HashMap<u64, u32>,
    hand: u32,
    evicted: u64,
}

impl Store {
    fn new(class: Class, base: u32, len: u32) -> Store {
        Store {
            class,
            base,
            slots: vec![Slot::default(); len as usize].into_boxed_slice(),
            map: HashMap::with_capacity(len as usize),
            hand: 0,
            evicted: 0,
        }
    }

    /// Whether `addr` is already cached at exactly `reg`; re-admitting it would rewrite
    /// bytes that are already there.
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
    /// duration of the write. `None` when the slot holding `addr` is already being
    /// written, or every slot is: a reason to decline and never a reason to wait.
    fn claim(&mut self, addr: u64, reg: Register) -> Option<u32> {
        let n = self.slots.len() as u32;
        if n == 0 {
            return None;
        }
        let i = match self.map.get(&addr) {
            Some(&i) if !self.slots[i as usize].busy => i,
            Some(_) => return None,
            None => self.evict()?,
        };
        self.map.insert(addr, i);
        // A fresh entry starts with a clear reference bit: it already passed admission,
        // and a grace period would make the hand step over genuinely hot entries to
        // reach it.
        self.slots[i as usize] = Slot {
            addr,
            reg,
            used: false,
            live: false,
            busy: true,
        };
        Some(i)
    }

    /// The CLOCK hand. Two sweeps at most: the first clears reference bits, the second
    /// is guaranteed to find one clear unless every slot is busy.
    fn evict(&mut self) -> Option<u32> {
        let n = self.slots.len() as u32;
        for _ in 0..2 * n {
            let i = self.hand;
            self.hand = (self.hand + 1) % n;
            let s = &mut self.slots[i as usize];
            if s.busy {
                continue;
            }
            if !s.live {
                return Some(i);
            }
            if s.used {
                s.used = false;
                continue;
            }
            let old = s.addr;
            s.live = false;
            self.map.remove(&old);
            self.evicted += 1;
            return Some(i);
        }
        None
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

// ---------------------------------------------------------------------------
// per-core state
// ---------------------------------------------------------------------------

/// Everything the cache keeps on one core.
///
/// Two different shardings meet here. The sketch and the stores are reached on the core
/// owning the address's consensus group, so they ride the hop the round is already
/// taking and the sketch sees the whole read stream. The hints are reached on whatever
/// core handles the request, because that is where the reply trailer carrying the width
/// lands and where the decision to take a cached leg is made; a hint is advisory, so a
/// per-core view of it is preferable anyway.
struct Local {
    sketch: Sketch,
    stores: [Store; 2],
    hints: Box<[u8]>,
    decayed: Instant,
    stats: Stats,
}

/// Per-core counters, read by [`Cache::local_stats`]; the exporter sums across cores.
#[derive(Clone, Copy, Default)]
pub struct Stats {
    pub hits: u64,
    pub misses: u64,
    /// Hits that then passed confirmation against the quorum. The gap between this and
    /// `hits` is what staleness costs.
    pub served: u64,
    pub admits: u64,
    pub evictions: u64,
    pub stale: u64,
    pub shed: u64,
}

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

pub struct Cache {
    alloc: &'static Allocator,
    disk: Disk,
    geo: Geometry,
    roster: config::Live<Roster>,
    state: Box<[RefCell<Local>]>,
}

// Sound because every `Local` is only ever borrowed from the worker that owns it, and
// never across an await. Same argument as `Allocator` and `Paxos`.
unsafe impl Sync for Cache {}

/// Build the cache and leak it: hop closures must be `Send + 'static`.
///
/// The region is whatever `layout::format` carved; a config that asked for no cache
/// produces a cache with no slots, which declines everything at no cost.
pub fn open(alloc: &'static Allocator, cores: usize) -> &'static Cache {
    let geo = alloc.geometry();
    let now = Instant::now();
    let state: Vec<RefCell<Local>> = (0..cores)
        .map(|c| {
            let stores = [Class::Small, Class::Huge].map(|class| {
                let (base, len) = stripe(geo.cache_slots(class), cores, c);
                Store::new(class, base, len)
            });
            RefCell::new(Local {
                sketch: Sketch::new(),
                stores,
                hints: vec![0u8; HINTS].into_boxed_slice(),
                decayed: now,
                stats: Stats::default(),
            })
        })
        .collect();
    Box::leak(Box::new(Cache {
        alloc,
        disk: alloc.disk(),
        geo,
        roster: config::Live::new(Roster::of(alloc.config())),
        state: state.into(),
    }))
}

/// The controller: `w(k) = min(ceil(q̂(k) / τ), W_max)`. Width proportional to the
/// measured rate is what makes zipf work — the head gets wide, the long tail gets zero,
/// and the total replica count stays sublinear in the key space.
fn width(q: u8, tau: u8, cap: u8) -> u8 {
    u8::try_from((q as u32).div_ceil(tau as u32))
        .unwrap_or(u8::MAX)
        .min(cap)
}

/// Hysteresis: raise fast, lower one step at a time. Rendezvous nesting means a one-step
/// change churns only the boundary replica, so damping the descent is the whole of the
/// anti-oscillation story.
fn damp(cur: u8, w: u8) -> u8 {
    if w >= cur {
        w
    } else {
        cur.saturating_sub(1).max(w)
    }
}

/// Core `c`'s contiguous share of `total` slots. Contiguous rather than striped so a
/// CLOCK sweep walks the device in order.
fn stripe(total: u64, cores: usize, c: usize) -> (u32, u32) {
    let total = total.min(u32::MAX as u64);
    let lo = total * c as u64 / cores as u64;
    let hi = total * (c + 1) as u64 / cores as u64;
    (lo as u32, (hi - lo) as u32)
}

impl Cache {
    fn local(&self) -> std::cell::RefMut<'_, Local> {
        self.state[runtime::core()].borrow_mut()
    }

    /// Re-derive the cohort roster. Control thread only, alongside `Allocator::install`.
    pub fn install(&self, cfg: &Config) {
        self.roster.install(Roster::of(cfg));
    }

    /// `τ`, in requests per decay interval. Zero disables the cache entirely and is the
    /// default: a node that has not been told a target rate has no business guessing
    /// one. Clamped to `COUNTER_MAX`, above which the sketch cannot reach `τ` anyway.
    fn tau(&self) -> u8 {
        let t = self.alloc.config().policy.cache_target_rate;
        t.clamp(0, COUNTER_MAX as u32) as u8
    }

    fn enabled(&self) -> bool {
        self.tau() > 0 && !self.roster.get().nodes.is_empty()
    }

    /// Sheds while the allocator is short of free space, or while the device's rate
    /// budget is already committed ahead. Cache space is statically separate from the
    /// allocator's but device bandwidth is not, so the cache stops admitting before
    /// anything authoritative slows down. An inbound extent migration reaches the cache
    /// through this and nothing else.
    pub fn shedding(&self) -> bool {
        self.alloc.pressure() != Pressure::Normal || self.alloc.device_pressed()
    }

    fn stat(&self, f: impl FnOnce(&mut Stats)) {
        f(&mut self.local().stats);
    }

    /// This core's counters. Evictions are the stores' own tally rather than a separate
    /// counter.
    pub fn local_stats(&self) -> Stats {
        let l = self.local();
        let mut s = l.stats;
        s.evictions = l.stores[0].evicted + l.stores[1].evicted;
        s
    }

    /// Count a cached page that passed confirmation. The one counter the cache cannot
    /// keep itself, because confirmation happens inside the consensus round.
    pub fn served(&self) {
        self.stat(|s| s.served += 1);
    }

    // ----------------------------------------------------------------- hotness

    /// Record one read of `addr` and return the width its owner should advertise.
    ///
    /// Must run on the core owning the address's group, where paxos already handles the
    /// metadata round, so the owner keeps observing the whole read stream even for pages
    /// it no longer serves itself.
    pub fn observe_local(&self, addr: GlobalAddr) -> u8 {
        let tau = self.tau();
        if tau == 0 {
            return 0;
        }
        let cap = self.w_max();
        width(self.local().sketch.observe(addr.0), tau, cap)
    }

    /// `observe_local` with a hop of its own, for the 4 MiB path: an immutable hit takes
    /// no metadata round, so there is no owner reply to carry a width and the reader's
    /// own read stream is the only signal.
    pub async fn observe(&'static self, addr: GlobalAddr) -> u8 {
        if !self.enabled() {
            return 0;
        }
        let Ok(owner) = self.alloc.owner_core(addr) else {
            return 0;
        };
        runtime::on_core(owner, move || async move { self.observe_local(addr) }).await
    }

    /// `W_max = min(cohort_size, 64)`.
    fn w_max(&self) -> u8 {
        u8::try_from(self.roster.get().nodes.len())
            .unwrap_or(W_CAP)
            .min(W_CAP)
    }

    /// Admission filter: the same sketch used TinyLFU-style, so a one-hit wonder never
    /// enters and never evicts anything that earned its slot.
    fn hot_enough(&self, addr: GlobalAddr) -> bool {
        let tau = self.tau();
        tau > 0 && self.local().sketch.estimate(addr.0) >= tau
    }

    // ------------------------------------------------------------------- hints

    /// The width last advertised for `addr`, as this core remembers it.
    ///
    /// The cached leg and the metadata round are issued together, but `w` arrives *in*
    /// that round's reply, so the leg can only be taken on a width learned from an
    /// earlier read. The first read of a key is therefore always uncached, which is also
    /// what the admission filter wants.
    pub fn hint(&self, addr: GlobalAddr) -> u8 {
        self.local().hints[Cache::hint_slot(addr)]
    }

    /// Record the width from a reply trailer, damped.
    pub fn note_hint(&self, addr: GlobalAddr, w: u8) {
        let i = Cache::hint_slot(addr);
        let c = &mut self.local().hints[i];
        *c = damp(*c, w);
    }

    fn hint_slot(addr: GlobalAddr) -> usize {
        config::mix(addr.0) as usize & (HINTS - 1)
    }

    // --------------------------------------------------------------- placement

    /// Whether we are one of the `w` replicas for `addr`: the number of cohort peers
    /// that outrank us is below the width.
    pub fn holds(&self, addr: GlobalAddr, w: u8) -> bool {
        let r = self.roster.get();
        if w == 0 || r.nodes.is_empty() {
            return false;
        }
        let mine = rank(addr.0, r.me);
        r.nodes.iter().filter(|&&n| rank(addr.0, n) > mine).count() < w as usize
    }

    /// The highest-ranked live member of `R`, excluding ourselves. `ok` is the
    /// reachability test: a cohort peer we hold no link to is not a candidate.
    pub fn replica(&self, addr: GlobalAddr, w: u8, ok: impl Fn(u32) -> bool) -> Option<u32> {
        let r = self.roster.get();
        if w == 0 {
            return None;
        }
        let mut best: Option<(u64, u32)> = None;
        for &n in &r.nodes {
            if n == r.me || !ok(n) {
                continue;
            }
            let s = rank(addr.0, n);
            if best.is_none_or(|(b, _)| s > b) {
                best = Some((s, n));
            }
        }
        let (score, node) = best?;
        let ahead = r.nodes.iter().filter(|&&n| rank(addr.0, n) > score).count();
        (ahead < w as usize).then_some(node)
    }

    // ------------------------------------------------------------------- store

    fn class(huge: bool) -> usize {
        usize::from(huge)
    }

    /// Read `addr` out of the local cache region into `buf`, returning the register the
    /// entry claims. The caller confirms that register against the quorum; this makes no
    /// claim about freshness.
    ///
    /// `off` is a byte offset into the page, for the 4 MiB class where one page is many
    /// block requests.
    pub async fn load(
        &'static self,
        addr: GlobalAddr,
        huge: bool,
        off: usize,
        buf: Buf,
    ) -> Option<Register> {
        self.load_at(addr, huge, off, buf, None).await
    }

    /// The cached 4 MiB path. An entry is servable only while it carries the current
    /// epoch's live version, so an epoch bump invalidates every immutable entry with one
    /// comparison — the whole of the invalidation protocol. The register comes back so
    /// the caller can confirm it against the group: this class has no per-page checksum
    /// and a 4 MiB frame has no trailer, so the version is the only handle on which
    /// value these bytes are.
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

    /// What our cached copy of `addr` claims, with no bytes moved: the register half of
    /// a 4 MiB cache hit, whose frame has no trailer to gather one into. Filtered
    /// exactly as [`Self::load_immutable`] filters, so the two agree on which entry they
    /// mean.
    pub async fn peek_immutable(&'static self, addr: GlobalAddr, huge: bool) -> Option<Register> {
        if !self.enabled() {
            return None;
        }
        let want = self.live_version(addr);
        let owner = self.alloc.owner_core(addr).ok()?;
        runtime::on_core(owner, move || async move {
            self.find_here(addr, huge, Some(want)).map(|(_, _, _, r)| r)
        })
        .await
    }

    /// Look the entry up on the core that owns it, then read the bytes here.
    ///
    /// The lookup has to hop and the IO must not: a registered buffer's index is
    /// meaningful only on the ring it was registered on, so `buf` may only be handed to
    /// the disk from the core the request arrived on. Same shape as the allocator's
    /// reserve-here, write-there split.
    ///
    /// `want` filters on the version *before* any IO, so a stale immutable entry never
    /// puts bytes in the caller's buffer.
    async fn load_at(
        &'static self,
        addr: GlobalAddr,
        huge: bool,
        off: usize,
        buf: Buf,
        want: Option<u64>,
    ) -> Option<Register> {
        let owner = self.alloc.owner_core(addr).ok()?;
        let (class, base, slot, reg) =
            runtime::on_core(
                owner,
                move || async move { self.find_here(addr, huge, want) },
            )
            .await?;
        if off as u64 + buf.len() as u64 > class.bytes() {
            return None;
        }
        let at = self.geo.cache_off(class, base + slot) + off as u64;
        if self.disk.read(at, buf).await.is_err() {
            // A cache page we cannot read is one we do not have. Silent rot is invisible
            // here — confirmation covers the register, not the bytes — but a hard read
            // error means the entry is gone.
            runtime::on_core(owner, move || async move {
                self.local().stores[Cache::class(huge)].forget(addr.0);
                self.stat(|s| s.misses += 1);
            })
            .await;
            return None;
        }
        self.stat(|s| s.hits += 1);
        Some(reg)
    }

    fn find_here(
        &self,
        addr: GlobalAddr,
        huge: bool,
        want: Option<u64>,
    ) -> Option<(Class, u32, u32, Register)> {
        let k = Cache::class(huge);
        // Bind before the borrow ends: a scrutinee's borrow lives as long as the arms,
        // and the miss arm reaches for the same cell again.
        let found = self.local().stores[k].find(addr.0);
        let Some((slot, reg)) = found.filter(|v| want.is_none_or(|w| v.1.version == w)) else {
            self.stat(|s| s.misses += 1);
            return None;
        };
        let l = self.local();
        Some((l.stores[k].class, l.stores[k].base, slot, reg))
    }

    /// Offer `buf` to the cache as the value of `addr` at `reg`, given the width `w` its
    /// owner last advertised.
    ///
    /// Declines silently — not one of the `w` replicas, not hot enough, device under
    /// pressure, or every slot being written. The cache never fails a write, it declines
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
        let Ok(owner) = self.alloc.owner_core(addr) else {
            return;
        };
        // Claim on the owning core, write here, then report back: the buffer's
        // registration belongs to this core's ring and cannot travel (see `load_at`).
        let Some((class, base, slot)) =
            runtime::on_core(
                owner,
                move || async move { self.claim_here(addr, huge, reg) },
            )
            .await
        else {
            return;
        };
        // One IO, in place, `Buffered`: no torn-write hazard, because nothing points at
        // these bytes after a restart.
        let ok = buf.len() as u64 == class.bytes() && {
            let off = self.geo.cache_off(class, base + slot);
            self.disk
                .write(off, buf, Durability::Buffered)
                .await
                .is_ok()
        };
        runtime::on_core(owner, move || async move {
            self.local().stores[Cache::class(huge)].finish(slot, ok);
            if ok {
                self.stat(|s| s.admits += 1);
            }
        })
        .await;
    }

    fn claim_here(&self, addr: GlobalAddr, huge: bool, reg: Register) -> Option<(Class, u32, u32)> {
        let k = Cache::class(huge);
        if self.local().stores[k].current(addr.0, reg) {
            return None;
        }
        if self.shedding() {
            self.stat(|s| s.shed += 1);
            return None;
        }
        // The one-hit-wonder filter, but only where this node's sketch is the signal. A
        // 4 MiB page has no owner round to carry a width, so the reader's own estimate
        // is all there is; a 4 KiB page arrives with the width its owner computed from
        // the whole read stream.
        if huge && !self.hot_enough(addr) {
            return None;
        }
        let mut l = self.local();
        let slot = l.stores[k].claim(addr.0, reg)?;
        Some((l.stores[k].class, l.stores[k].base, slot))
    }

    /// Drop an entry that failed confirmation. Cheap enough to call speculatively.
    pub async fn forget(&'static self, addr: GlobalAddr, huge: bool) {
        let Ok(owner) = self.alloc.owner_core(addr) else {
            return;
        };
        runtime::on_core(owner, move || async move {
            let k = Cache::class(huge);
            self.local().stores[k].forget(addr.0);
            self.stat(|s| s.stale += 1);
        })
        .await;
    }

    // -------------------------------------------------------------- immutable

    /// The version an Immutable page holds while it is live at its volume's epoch.
    ///
    /// Only quorum-confirmed values are admitted for this class, and one version has
    /// exactly one such value, so a version is a complete identity for a cached 4 MiB
    /// page — no ballot beside it, which matters because a 4 MiB frame has no trailer to
    /// carry one in.
    fn live_version(&self, addr: GlobalAddr) -> u64 {
        3 * self.alloc.config().tombstone_epoch_of(addr.0) + 1
    }

    // ------------------------------------------------------------------- tick

    /// The decay. Driven from `Handler::tick`, which is a poll and not a timer: an idle
    /// worker takes no ticks, so the halving count comes from elapsed time rather than
    /// being assumed to be one.
    pub fn tick(&self, now: Instant) {
        let mut l = self.local();
        let elapsed = now.saturating_duration_since(l.decayed);
        let steps = (elapsed.as_nanos() / DECAY.as_nanos()) as u32;
        if steps == 0 {
            return;
        }
        l.decayed += DECAY * steps;
        l.sketch.halve(steps);
    }
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sketch_is_one_sided_and_decays() {
        let mut s = Sketch::new();
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
        let mut s = Sketch::new();
        // Two adjacent cells in row 0, forced by construction rather than by hashing.
        s.set(0, 0, 15);
        s.set(0, 1, 0);
        s.halve(1);
        assert_eq!(s.get(0, 0), 7);
        assert_eq!(s.get(0, 1), 0);
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

    #[test]
    fn clock_gives_a_second_chance() {
        let mut st = Store::new(Class::Small, 0, 2);
        let r = Register::default();
        let a = st.claim(1, r).unwrap();
        st.finish(a, true);
        let b = st.claim(2, r).unwrap();
        st.finish(b, true);

        // Touch 1 so it carries a reference bit, then admit a third page.
        assert!(st.find(1).is_some());
        let c = st.claim(3, r).unwrap();
        st.finish(c, true);

        assert!(st.find(1).is_some(), "referenced entry must survive");
        assert!(st.find(2).is_none(), "unreferenced entry must be evicted");
        assert!(st.find(3).is_some());
    }

    #[test]
    fn store_never_hands_out_a_busy_slot() {
        let mut st = Store::new(Class::Small, 0, 1);
        let r = Register::default();
        let i = st.claim(1, r).unwrap();
        // The single slot is in flight, so there is nothing to hand out.
        assert!(st.claim(2, r).is_none());
        st.finish(i, false);
        // A failed write leaves no entry behind.
        assert!(st.find(1).is_none());
        assert!(st.claim(2, r).is_some());
    }

    #[test]
    fn empty_store_declines() {
        let mut st = Store::new(Class::Huge, 0, 0);
        assert!(st.claim(1, Register::default()).is_none());
        assert!(st.find(1).is_none());
    }

    #[test]
    fn width_tracks_rate_and_clamps() {
        // Zero only at zero count, then one replica per multiple of the target rate.
        assert_eq!(width(0, 4, 8), 0);
        assert_eq!(width(1, 4, 8), 1);
        assert_eq!(width(4, 4, 8), 1);
        assert_eq!(width(5, 4, 8), 2);
        // W_max binds above the rate; a τ larger than the count still yields one.
        assert_eq!(width(15, 1, 8), 8);
        assert_eq!(width(15, 0xff, 8), 1);
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
    fn roster_is_this_node_s_cohort_column() {
        let mut cfg = crate::config::Config::default();
        cfg.node.id = 5;
        cfg.node.cohort = 1;
        // Member index is the cohort index, so column 1 is ours.
        cfg.topology.catalog = vec![[1, 5, 9], [2, 5, 10], [3, 7, 11]];
        let r = Roster::of(&cfg);
        assert_eq!(r.me, 5);
        let mut nodes = r.nodes.to_vec();
        nodes.sort_unstable();
        assert_eq!(nodes, vec![5, 7]);
    }

    #[test]
    fn stripes_cover_every_slot_exactly_once() {
        for (total, cores) in [(0u64, 4usize), (1, 4), (7, 4), (1024, 3), (100, 7)] {
            let mut next = 0;
            for c in 0..cores {
                let (base, len) = stripe(total, cores, c);
                assert_eq!(base, next);
                next += len;
            }
            assert_eq!(next as u64, total);
        }
    }
}
