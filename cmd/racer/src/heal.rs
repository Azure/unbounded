//! Anti-entropy: per-group digests, the wire encoding of the three group-addressed
//! control ops, and the sweep that drives them. Join replay and periodic digest
//! exchange are one mechanism: enumerate a group's registers and hand the differences
//! to `paxos::repair`.
//!
//! The stream carries no page bytes: a cursor emits `(addr, version, ballot)` and the
//! receiver pulls the page with a `GET`, which verifies its CRC at the source, so a page
//! changing mid-stream arrives newer, not torn. The tree is one flat level of 512
//! buckets, one 4 KiB trailer, so a root would only add a round trip.

use std::cell::{Cell, RefCell};
use std::collections::BTreeMap;
use std::time::{Duration, Instant};

use crate::alloc::{Allocator, GlobalAddr, Pressure, Status};
use crate::config::{self, Config, GroupId};
use crate::fabric::{self, Bucket, Class as Klass, Cmd, Footer, GroupIx, Link, Seq};
use crate::layout::{Class, Entry};
use crate::paxos::{Ballot, Paxos, Register};
use crate::runtime::{self, CoreId, PoolBuf};
use crate::server::{self, Server};

/// Maps a page address to its consensus group; the slab holds no config to do it with.
pub(crate) type Groups<'a> = dyn Fn(u64) -> GroupId + 'a;

// --- shape ---

/// Buckets per group. 512 u64 fills one 4 KiB trailer, so a comparison is one round
/// trip. The wire owns the number; this is the name the sweep uses.
pub(crate) const BUCKETS: usize = fabric::BUCKETS;

/// `(addr, version, ballot)` triples in one chunk.
const TUPLES: usize = fabric::SnapNextReply::CAPACITY;

/// Concurrent cursors one slab holds open; also bounds deferred reclamation.
const MAX_SNAPS: usize = 8;

/// Entries one `snap_next` may scan per frame, so a sparse filter cannot walk a shard.
const SNAP_SCAN: u32 = 1 << 20;

/// Registers one slab retains before failing its cursors; past this, failing beats
/// pinning the device. Retention grows as `write_rate x cursor_lifetime`.
const MAX_RETAINED: usize = 1 << 16;

/// A cursor nobody has advanced for this long is abandoned and its slots released.
const SNAP_TTL: Duration = Duration::from_secs(30);

/// The bucket a page's digest lands in: hash bits 14..23, vs 0..14 for `config::slot_of`,
/// so a group's pages spread over all buckets.
pub(crate) fn bucket_of(addr: u64) -> u16 {
    ((config::mix(addr) >> 14) & (BUCKETS as u64 - 1)) as u16
}

/// What a cursor emits. A predicate during the walk, not an index, so it costs a full pass.
#[derive(Clone, Copy, PartialEq, Debug)]
pub(crate) enum Filter {
    All,
    /// One digest bucket: the anti-entropy comparison.
    Bucket(u16),
    /// A half-open span `lo..hi`; universe and base concatenate, so one extent is one span.
    Range {
        lo: u64,
        hi: u64,
    },
}

impl Filter {
    fn keeps(&self, addr: u64) -> bool {
        match *self {
            Filter::All => true,
            Filter::Bucket(b) => bucket_of(addr) == b,
            Filter::Range { lo, hi } => addr >= lo && addr < hi,
        }
    }
}

/// Consensus groups across every universe this node is in.
fn total_groups(cfg: &Config) -> u32 {
    cfg.universes().iter().map(|u| u.catalog.len() as u32).sum()
}

/// The `n`th group in that flat sequence, universes in id order. One sequence, not one
/// per universe, so a node in two universes splits the budget by groups held.
fn group_at(cfg: &Config, mut n: u32) -> Option<GroupId> {
    for u in cfg.universes() {
        let len = u.catalog.len() as u32;
        if n < len {
            return Some(GroupId::new(u.id, n));
        }
        n -= len;
    }
    None
}

/// The leaf tuple. Not cryptographic: a digest match is a hint, never proof of equality.
fn leaf(addr: u64, version: u64, ballot: u64, crc: u32) -> u64 {
    config::mix(addr ^ config::mix(version ^ config::mix(ballot ^ crc as u64)))
}

// --- digests ---

/// Per-group XOR accumulators over the registers this node owns. XOR updates in two
/// calls with no ordering to preserve. Keyed by `(group, class)`, one per slab, not by
/// shard: the shard depends on the local core count, so two nodes would disagree on it.
#[derive(Default)]
pub(crate) struct Digests {
    groups: BTreeMap<GroupId, Box<[u64; BUCKETS]>>,
}

impl Digests {
    /// Take a register out or put one in; XOR is its own inverse. `data_crc` is zero on
    /// 4 MiB entries, which carry no checksum, so one tuple shape serves both classes.
    pub(crate) fn toggle(&mut self, group: GroupId, e: &Entry) {
        let v = self
            .groups
            .entry(group)
            .or_insert_with(|| Box::new([0; BUCKETS]));
        v[bucket_of(e.addr) as usize] ^= leaf(e.addr, e.version, e.ballot, e.data_crc);
    }

    /// Digest vector for a group; all zeroes if we hold nothing for it.
    pub(crate) fn vector(&self, group: GroupId) -> Box<[u64; BUCKETS]> {
        self.groups
            .get(&group)
            .cloned()
            .unwrap_or_else(|| Box::new([0; BUCKETS]))
    }

    /// Groups this node holds registers for; minus the catalog's, exactly what to shed.
    pub(crate) fn held(&self) -> Vec<GroupId> {
        self.groups.keys().copied().collect()
    }

    /// Drop a group whose accumulator is back to zero; `toggle` never prunes, since that
    /// would cost a scan of the vector on the hot path.
    pub(crate) fn forget(&mut self, group: GroupId) {
        if self
            .groups
            .get(&group)
            .is_some_and(|v| v.iter().all(|&d| d == 0))
        {
            self.groups.remove(&group);
        }
    }
}

// --- cursors ---

/// A register as the cursor emits it; no page bytes, the receiver pulls those with `GET`.
pub(crate) type Tuple = (u64, Register);

fn tuple(e: &Entry) -> Tuple {
    (
        e.addr,
        Register {
            version: e.version,
            ballot: Ballot::from_raw(e.ballot as u32),
        },
    )
}

/// A cursor id: `generation | core | class | slot`, so `SNAPNEXT` needs only the id.
fn snap_id(core: usize, huge: bool, slot: usize, era: u32) -> u32 {
    era << 14 | (core as u32 & 0x3ff) << 4 | (huge as u32) << 3 | slot as u32 & 7
}

pub(crate) fn snap_parts(id: u32) -> (usize, bool, usize, u32) {
    (
        (id >> 4 & 0x3ff) as usize,
        id >> 3 & 1 == 1,
        (id & 7) as usize,
        id >> 14,
    )
}

/// One open enumeration of a group's registers on one slab.
struct Snap {
    group: GroupId,
    filter: Filter,
    era: u32,
    /// Next local entry index. Monotone, so the walk is one slab pass.
    cursor: u32,
    /// Where this cursor's view of `retained` begins: everything freed after it opened.
    drained: usize,
    /// Last chunk answered, so a retried `SNAPNEXT` repeats rather than skips.
    seq: u8,
    last: Vec<Tuple>,
    last_done: bool,
    touched: Instant,
}

/// The open cursors on one slab, plus the reclamation they hold back. Deferral is a
/// cursor's atomicity: a slot freed under an open cursor is neither reused nor
/// forgotten, so a page overwritten mid-walk still reports its value at open.
#[derive(Default)]
pub(crate) struct Snaps {
    open: [Option<Snap>; MAX_SNAPS],
    live: usize,
    era: u32,
    /// Local slot ids freed while a cursor was open; freed for real when the last closes.
    deferred: Vec<u32>,
    /// Registers of freed entries in release order; a cursor emits the tail after open.
    retained: Vec<Tuple>,
    /// Retention blew past `MAX_RETAINED`; every open cursor is void and says so.
    void: bool,
}

impl Snaps {
    fn busy(&self) -> bool {
        self.live > 0
    }

    /// Called from `Slab::set`, the one place an entry is overwritten or cleared.
    pub(crate) fn retain(&mut self, e: &Entry) {
        if !self.busy() {
            return;
        }
        if self.retained.len() >= MAX_RETAINED {
            self.void = true;
            return;
        }
        self.retained.push(tuple(e));
    }

    /// A freed slot is parked while a cursor is open, freed otherwise; void cursors park
    /// none.
    pub(crate) fn park(&mut self, free: &mut Vec<u32>, local: u32) {
        if self.busy() && !self.void {
            self.deferred.push(local);
        } else {
            free.push(local);
        }
    }

    pub(crate) fn start(
        &mut self,
        core: usize,
        huge: bool,
        group: GroupId,
        filter: Filter,
        now: Instant,
    ) -> Option<u32> {
        let slot = self.open.iter().position(|s| s.is_none())?;
        self.era = self.era.wrapping_add(1) & 0x3ffff;
        self.live += 1;
        let drained = self.retained.len();
        self.open[slot] = Some(Snap {
            group,
            filter,
            era: self.era,
            cursor: 0,
            drained,
            seq: u8::MAX,
            last: Vec::new(),
            last_done: false,
            touched: now,
        });
        Some(snap_id(core, huge, slot, self.era))
    }

    /// Next chunk; a repeated `seq` re-answers the previous one. `universe` is the
    /// namespace the request arrived on, `None` locally: ids are guessable, so a cursor
    /// answers only its own universe.
    pub(crate) fn next(
        &mut self,
        id: u32,
        seq: Option<u8>,
        universe: Option<u32>,
        entries: &[Entry],
        gof: &Groups,
        now: Instant,
    ) -> Result<(Vec<Tuple>, bool), Status> {
        let (_, _, slot, era) = snap_parts(id);
        if self.void {
            return Err(Status::NoSpace);
        }
        let retained = &self.retained;
        let s = match self.open.get_mut(slot) {
            Some(Some(s)) if s.era == era => s,
            _ => return Err(Status::Unmapped),
        };
        if universe.is_some_and(|u| u != s.group.universe()) {
            return Err(Status::Unmapped);
        }
        s.touched = now;
        if seq.is_some_and(|q| q == s.seq) {
            return Ok((s.last.clone(), s.last_done));
        }

        let (group, filter) = (s.group, s.filter);
        let keep = |addr: u64| gof(addr) == group && filter.keeps(addr);
        let mut out = Vec::with_capacity(TUPLES);
        let mut scanned = 0;
        while (s.cursor as usize) < entries.len() && out.len() < TUPLES && scanned < SNAP_SCAN {
            let e = &entries[s.cursor as usize];
            s.cursor += 1;
            scanned += 1;
            if e.addr != 0 && keep(e.addr) {
                out.push(tuple(e));
            }
        }
        // Retained values drain last; they are older and the receiver applies if newer.
        let walked = s.cursor as usize >= entries.len();
        if walked {
            while s.drained < retained.len() && out.len() < TUPLES {
                let t = retained[s.drained];
                s.drained += 1;
                if keep(t.0) {
                    out.push(t);
                }
            }
        }
        let done = walked && s.drained >= retained.len();
        if let Some(q) = seq {
            s.seq = q;
            s.last = out.clone();
            s.last_done = done;
        }
        Ok((out, done))
    }

    pub(crate) fn stop(&mut self, id: u32, free: &mut Vec<u32>) {
        let (_, _, slot, era) = snap_parts(id);
        if matches!(self.open.get(slot), Some(Some(s)) if s.era == era) {
            self.open[slot] = None;
            self.live -= 1;
            self.settle(free);
        }
    }

    /// Abandon cursors nobody has advanced, so a dead peer cannot pin reclamation.
    pub(crate) fn expire(&mut self, now: Instant, free: &mut Vec<u32>) {
        for s in self.open.iter_mut() {
            if s.as_ref()
                .is_some_and(|s| now.duration_since(s.touched) > SNAP_TTL)
            {
                *s = None;
                self.live -= 1;
            }
        }
        self.settle(free);
    }

    /// Reclamation resumes once the last cursor is gone.
    fn settle(&mut self, free: &mut Vec<u32>) {
        if self.busy() {
            return;
        }
        free.append(&mut self.deferred);
        self.retained.clear();
        self.void = false;
    }
}

// --- the wire ---

/// The three group-addressed ops name a *group*, not a page, so `server::addr_of` is not
/// on their path and the universe is the namespace the frame arrived on: the target
/// rebuilds the [`GroupId`] locally. Their layout lives in `fabric`; what is here is the
/// translation between a sweep's own vocabulary and the wire's.

/// The class a group is being swept at, as the wire names it.
fn klass(huge: bool) -> Klass {
    Klass::of(huge)
}

/// One chunk of a cursor stream, folded into the map being built. Returns whether the
/// stream is finished.
fn take(chunk: &fabric::SnapNextReply, out: &mut BTreeMap<u64, Register>) -> bool {
    for t in chunk.tuples() {
        out.insert(t.addr, t.reg.into());
    }
    chunk.done
}

// --- the sweep ---

/// Differing buckets one sweep reconciles; the rest waits for the next pass.
const BUCKETS_PER_JOB: usize = 8;

/// Repairs one sweep will issue; each is a full prepare round against the whole group.
const REPAIRS_PER_JOB: usize = 64;

/// Repair budget while replaying, and the per-call push budget in `push_extent`. A
/// joining node has the whole group to fetch and, since it neither proposes nor accepts
/// until caught up, no write path to protect. The prepare round per address is not
/// avoidable: a register held by one member alone was not necessarily chosen. This is
/// the one budget the operator sets (`Policy::repairs_per_replay`): the rate a member
/// replacement runs at, the window a group spends at two of three, and the rate an
/// extent moves zones.
fn repairs_per_replay(cfg: &Config) -> usize {
    cfg.policy.repairs_per_replay as usize
}

/// Tuples one side of a bucket comparison will buffer; a larger bucket is skipped.
const MAX_BUCKET: usize = 1 << 16;

/// Shortest gap between sweeps on one core.
const INTERVAL: Duration = Duration::from_secs(1);

/// Registers one sweep offers back; each costs a `GETMETA` at all three new members.
const DROPS_PER_JOB: usize = 64;

#[derive(Clone, Copy, Default)]
pub struct Stats {
    pub sweeps: u64,
    pub buckets_diff: u64,
    pub repairs: u64,
    pub failed: u64,
    pub oversized: u64,
    pub dropped: u64,
}

/// One worker's share of the sweep: what it is doing, when it last did it, and where it
/// had got to. Lives in that worker's [`server::CoreState`], so no cell here is ever
/// reachable from another core and none of it needs to be shared.
#[derive(Default)]
pub(crate) struct Core {
    /// One sweep at a time per core: a job still running declines the next tick.
    busy: Cell<bool>,
    last: Cell<Option<Instant>>,
    /// Round-robin over the groups this core owns, so no group is starved.
    next: Cell<u32>,
    stats: RefCell<Stats>,
}

pub struct Heal {
    paxos: &'static Paxos,
}

pub fn open(paxos: &'static Paxos, _cores: usize) -> &'static Heal {
    Box::leak(Box::new(Heal { paxos }))
}

/// This worker's row, for the length of one synchronous step.
fn here<T>(f: impl FnOnce(&Core) -> T) -> T {
    runtime::here::<Server, T>(|ctx| f(&ctx.state().heal))
}

impl Heal {
    fn alloc(&self) -> &'static Allocator {
        self.paxos.alloc()
    }

    /// This core's counters; the exporter publishes a row per core and sums them.
    pub fn local_stats(&self) -> Stats {
        here(|c| *c.stats.borrow())
    }

    /// Groups being replayed into, and groups still holding registers they were moved out
    /// of, on this core. The control plane's only completion signal: it replaces one node
    /// at a time and must not start the next until this reads zero across the zone, or
    /// more groups sit at two of three and the space the next replacement needs is held.
    pub fn outstanding(&'static self) -> (u64, u64) {
        let replaying = self.paxos.replaying_here().len() as u64;
        let shedding = [false, true]
            .iter()
            .filter(|&&huge| self.serves(huge))
            .flat_map(|&huge| self.alloc().held_groups(huge))
            .filter(|&g| {
                self.paxos
                    .members(g)
                    .is_none_or(|m| self.paxos.self_index(&m).is_none())
            })
            .count() as u64;
        (replaying, shedding)
    }

    fn stat(&self, f: impl FnOnce(&mut Stats)) {
        here(|c| f(&mut c.stats.borrow_mut()));
    }

    /// Spawns a sweep; `Handler::tick` is synchronous. Declines under rate pressure, since
    /// the device budget is the write path's too. Free-space pressure does not decline:
    /// shedding is where space comes back from, so the sweep runs and skips anti-entropy.
    pub fn tick(&'static self, now: Instant) {
        let due = here(|c| {
            !c.busy.get()
                && !c
                    .last
                    .get()
                    .is_some_and(|t| now.duration_since(t) < INTERVAL)
        });
        if !due || self.alloc().store_pressed() {
            return;
        }
        here(|c| {
            c.last.set(Some(now));
            c.busy.set(true);
        });
        // The job stays on this core, so it clears the same row it just claimed.
        if !runtime::spawn(async move {
            let _ = self.sweep().await;
            here(|c| c.busy.set(false));
        }) {
            here(|c| c.busy.set(false));
        }
    }

    /// The next group `core` owns with live members, advancing that core's cursor past
    /// it. `None` once the scan comes back around having found none.
    fn next_group(&self, cfg: &Config, core: CoreId, c: &Core, groups: u32) -> Option<GroupId> {
        let cores = runtime::cores();
        let start = c.next.get();
        let mut n = start;
        loop {
            if let Some(cand) = group_at(cfg, n)
                && cand.index() as usize % cores == core.index()
                && self.paxos.members(cand).is_some()
            {
                c.next.set((n + 1) % groups);
                return Some(cand);
            }
            n = (n + 1) % groups;
            if n == start {
                return None;
            }
        }
    }

    /// One group per sweep, both classes, one peer. Everything past the first frame needs
    /// a digest mismatch, so a converged cluster pays one MERKLE per group and class.
    async fn sweep(&'static self) -> Result<(), Status> {
        let cfg = server::config();
        let groups = total_groups(&cfg);
        if groups == 0 {
            return Ok(());
        }

        // Shedding first, regardless of free space: everything below competes for it.
        for huge in [false, true] {
            if self.serves(huge) {
                self.shed(huge).await;
            }
        }
        if self.alloc().pressure() != Pressure::Normal {
            return Ok(());
        }

        // Pick a group whose paxos core is this one, so `repair` and the allocator shard
        // are local. A group already replaying goes first; round-robin alone would give
        // it one budget every `groups / cores` intervals.
        let g = match self
            .paxos
            .replaying_here()
            .into_iter()
            .find(|&g| self.paxos.members(g).is_some())
        {
            Some(r) => r,
            // The cursor is this core's, so finding a group and advancing past it is one
            // step: two sweeps must not pick up from the same place.
            None => {
                let picked = runtime::here::<Server, _>(|ctx| {
                    self.next_group(&cfg, ctx.core(), &ctx.state().heal, groups)
                });
                match picked {
                    Some(g) => g,
                    None => return Ok(()),
                }
            }
        };

        self.stat(|s| s.sweeps += 1);
        // Replay is sticky: one repaired bucket makes our side non-empty, so re-detection
        // would clear the flag early. Only a comparison with no repairs left ends it.
        let was = self.paxos.replaying(g).await;
        let mut replaying = false;
        let mut checked = true;
        for huge in [false, true] {
            if !self.serves(huge) {
                continue;
            }
            match self.compare(&cfg, g, huge, was).await {
                Ok(r) => replaying |= r,
                Err(e) => {
                    checked = false;
                    self.stat(|s| s.failed += 1);
                    let _ = e;
                }
            }
        }
        // A class we could not compare is not evidence of having caught up on it.
        let still = replaying || (was && !checked);
        if still {
            self.paxos.set_replaying(g, true).await;
        } else if was && self.paxos.rejoin(g).await.is_err() {
            // Leaving a replay needs the promise recovered first; the next sweep retries.
            self.stat(|s| s.failed += 1);
        }
        self.hand_over(&cfg, g).await;
        Ok(())
    }

    /// The source side of an extent migration; the zone handing the extent over pushes,
    /// because the destination holds neither this zone's slot table nor its catalog. Every
    /// member pushes its own copy and the stream is apply-if-newer, so the copies need no
    /// ordering. The seal comes first and freezes the extent here, so a page pushed after
    /// it cannot be overtaken by a write we accept. Pushing repeats until the control
    /// plane flips `zone` and clears `next_zone`, the only completion signal, so a
    /// destination down for the first pass still converges.
    async fn hand_over(&'static self, cfg: &Config, group: GroupId) {
        // Only this group's own universe: a group holds registers for nothing else.
        let Some(u) = cfg.universe(group.universe()) else {
            return;
        };
        for ext in &u.extents {
            if ext.zone != cfg.node.zone || ext.next_zone == 0 {
                continue;
            }
            let lo = GlobalAddr::new(u.id, ext.base_lba);
            let hi = GlobalAddr::new(u.id, ext.end_lba());
            if !self.paxos.sealed(ext.id).await {
                // Push next tick: a seal only just chosen may not have reached the members
                // whose copies we are about to walk.
                let _ = self.paxos.seal_extent(lo, ext.id).await;
                continue;
            }
            // One stuck extent must not stop the others; the next tick retries it.
            let filter = Filter::Range { lo: lo.0, hi: hi.0 };
            let _ = self
                .push_extent(group, ext.huge, filter, ext.next_zone)
                .await;
        }
    }

    /// Push this group's registers for one extent to the zone taking it over.
    async fn push_extent(
        &'static self,
        group: GroupId,
        huge: bool,
        filter: Filter,
        zone: u32,
    ) -> Result<(), Status> {
        let snap = self.alloc().snap_open(group, huge, filter).await?;
        let mut sent = 0usize;
        loop {
            let (tuples, done) = snap.next().await?;
            for (addr, r) in tuples {
                let _ = self.paxos.push(GlobalAddr(addr), r, zone).await;
                sent += 1;
            }
            if done || sent >= repairs_per_replay(&server::config()) {
                break;
            }
        }
        snap.close().await;
        Ok(())
    }

    /// Whether this node has a slab of the class, by device geometry rather than config.
    fn serves(&self, huge: bool) -> bool {
        let c = if huge { Class::Huge } else { Class::Small };
        self.alloc().geometry().slots(c) > 0
    }

    // --- the shed ---

    /// Give back the registers of groups this node is no longer in the catalog for. The
    /// leaving node holds every register it ever accepted, since nothing on the write path
    /// revisits a page once placed; without this a node only ever grows. The digest map
    /// names the work: its groups minus the ones the catalog still names us in.
    async fn shed(&'static self, huge: bool) {
        let orphans: Vec<GroupId> = self
            .alloc()
            .held_groups(huge)
            .into_iter()
            .filter(|&g| {
                self.paxos
                    .members(g)
                    .is_none_or(|m| self.paxos.self_index(&m).is_none())
            })
            .collect();
        let mut budget = DROPS_PER_JOB;
        for g in orphans {
            if budget == 0 {
                return;
            }
            // One group that will not drain must not block the others.
            if self.drain(g, huge, &mut budget).await.is_err() {
                self.stat(|s| s.failed += 1);
            }
        }
    }

    /// Walk one orphaned group and forget every register its new members can be shown to
    /// hold. A register nobody confirmed stays, so a degraded window costs availability,
    /// not the value.
    async fn drain(
        &'static self,
        group: GroupId,
        huge: bool,
        budget: &mut usize,
    ) -> Result<(), Status> {
        let snap = self.alloc().snap_open(group, huge, Filter::All).await?;
        let mut walked = false;
        while *budget > 0 {
            let (tuples, done) = snap.next().await?;
            for (addr, r) in tuples {
                if *budget == 0 {
                    break;
                }
                *budget -= 1;
                let a = GlobalAddr(addr);
                if self.paxos.confirmed(a, r.version).await
                    && self.alloc().discard(a, r.version).await.is_ok()
                {
                    self.stat(|s| s.dropped += 1);
                }
            }
            if done {
                walked = true;
                break;
            }
        }
        snap.close().await;
        // Only a full pass proves the group is empty, and `forget` re-checks anyway.
        if walked {
            self.alloc().forget_group(group, huge).await;
        }
        Ok(())
    }

    /// Whether this group is still replaying; only a clean comparison clears `was`.
    async fn compare(
        &'static self,
        cfg: &Config,
        group: GroupId,
        huge: bool,
        was: bool,
    ) -> Result<bool, Status> {
        let m = self.paxos.members(group).ok_or(Status::Unmapped)?;
        let me = self.paxos.self_index(&m).ok_or(Status::Unmapped)?;
        // The partner alternates with group parity, so the group walks all three edges.
        let from = ((me as u32 + 1 + group.index() % 2) % 3) as u8;
        let link = self
            .paxos
            .link_of(group.universe(), m[from as usize])
            .ok_or(Status::Io)?;

        let mine = self.alloc().digests(group, huge).await;
        let t = PoolBuf::alloc(fabric::BLOCK).await;
        let ask = Cmd::Merkle {
            group: GroupIx::new(group.index()).ok_or(Status::Unmapped)?,
            class: klass(huge),
        };
        link.send(ask, t.buf()).await.map_err(Status::from_wire)?;
        let theirs = fabric::MerkleReply::decode(&t)
            .map_err(Status::from_wire)?
            .0;

        // Our side empty against a peer with data is a node joining, not a divergence:
        // same work, different budget, and we refuse accepts until the digests agree.
        let replay = was || (mine.iter().all(|&d| d == 0) && theirs.iter().any(|&d| d != 0));
        if replay {
            self.paxos.set_replaying(group, true).await;
        }

        let diff: Vec<u16> = (0..BUCKETS)
            .filter(|&i| mine[i] != theirs[i])
            .map(|i| i as u16)
            .take(if replay { BUCKETS } else { BUCKETS_PER_JOB })
            .collect();
        if diff.is_empty() {
            // Nothing on either side the other lacks: the only thing that ends a replay.
            return Ok(false);
        }
        self.stat(|s| s.buckets_diff += diff.len() as u64);

        let mut budget = if replay {
            repairs_per_replay(cfg)
        } else {
            REPAIRS_PER_JOB
        };
        for b in diff {
            if budget == 0 {
                break;
            }
            self.reconcile(cfg, group, huge, link, b, &mut budget)
                .await?;
        }
        Ok(replay)
    }

    /// Enumerate one bucket on both sides and repair every address they disagree on, in
    /// either direction. The registers are only a difference test: `repair` re-derives
    /// the truth with a prepare round, so a cursor that raced a write costs an extra
    /// repair, not a wrong one.
    async fn reconcile(
        &'static self,
        cfg: &Config,
        group: GroupId,
        huge: bool,
        link: &Link,
        bucket: u16,
        budget: &mut usize,
    ) -> Result<(), Status> {
        let Some(theirs) = self.remote_bucket(link, group, huge, bucket).await? else {
            self.stat(|s| s.oversized += 1);
            return Ok(());
        };
        let Some(mine) = self.local_bucket(group, huge, bucket).await? else {
            self.stat(|s| s.oversized += 1);
            return Ok(());
        };

        for (addr, r) in theirs.iter() {
            if *budget == 0 {
                return Ok(());
            }
            if mine.get(addr) != Some(r) {
                *budget -= 1;
                self.repair(cfg, GlobalAddr(*addr)).await;
            }
        }
        for addr in mine.keys() {
            if *budget == 0 {
                return Ok(());
            }
            if !theirs.contains_key(addr) {
                *budget -= 1;
                self.repair(cfg, GlobalAddr(*addr)).await;
            }
        }
        Ok(())
    }

    async fn repair(&'static self, cfg: &Config, addr: GlobalAddr) {
        // A page whose extent left the config no longer exists; not a divergence.
        if cfg.extent_at(addr.0).is_none() {
            return;
        }
        match self.paxos.repair(addr).await {
            Ok(_) => self.stat(|s| s.repairs += 1),
            Err(_) => self.stat(|s| s.failed += 1),
        }
    }

    /// Pull one bucket from a peer. A cursor abandoned here is left to the TTL in
    /// `Snaps::expire`; a finished one is released by the server on the last chunk.
    async fn remote_bucket(
        &'static self,
        link: &Link,
        group: GroupId,
        huge: bool,
        bucket: u16,
    ) -> Result<Option<BTreeMap<u64, Register>>, Status> {
        let t = PoolBuf::alloc(fabric::BLOCK).await;
        let open = Cmd::SnapOpen {
            group: GroupIx::new(group.index()).ok_or(Status::Unmapped)?,
            class: klass(huge),
            bucket: Some(Bucket::new(bucket).ok_or(Status::Unmapped)?),
        };
        link.send(open, t.buf()).await.map_err(Status::from_wire)?;
        let cursor = fabric::SnapOpenReply::decode(&t)
            .map_err(Status::from_wire)?
            .cursor;

        let mut out = BTreeMap::new();
        // The sequence is six bits wide and the walk may need more chunks than that, so
        // it cycles: it only has to tell a chunk from the one before it and from a retry.
        for seq in 0..=u8::MAX {
            let t = PoolBuf::alloc(fabric::BLOCK).await;
            let next = Cmd::SnapNext {
                cursor,
                seq: Seq::wrap(seq),
            };
            link.send(next, t.buf()).await.map_err(Status::from_wire)?;
            let chunk = fabric::SnapNextReply::decode(&t).map_err(Status::from_wire)?;
            if take(&chunk, &mut out) {
                return Ok(Some(out));
            }
            if out.len() > MAX_BUCKET {
                return Ok(None);
            }
        }
        Ok(None)
    }

    async fn local_bucket(
        &'static self,
        group: GroupId,
        huge: bool,
        bucket: u16,
    ) -> Result<Option<BTreeMap<u64, Register>>, Status> {
        let snap = self
            .alloc()
            .snap_open(group, huge, Filter::Bucket(bucket))
            .await?;
        let mut out = BTreeMap::new();
        let mut over = false;
        loop {
            let (tuples, done) = snap.next().await?;
            for (addr, r) in tuples {
                out.insert(addr, r);
            }
            if done {
                break;
            }
            if out.len() > MAX_BUCKET {
                over = true;
                break;
            }
        }
        snap.close().await;
        Ok((!over).then_some(out))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::layout::State;

    fn entry(addr: u64, version: u64) -> Entry {
        Entry {
            addr,
            version,
            ballot: 1,
            state: State::Live,
            ..Entry::default()
        }
    }

    /// Toggle is its own inverse, so order does not matter and a change undone leaves
    /// nothing behind.
    #[test]
    fn digests_are_a_set_not_a_history() {
        let g = |i: u32| GroupId::new(1, i);
        let (mut a, mut b) = (Digests::default(), Digests::default());
        let (x, y) = (entry(11, 1), entry(22, 5));
        a.toggle(g(0), &x);
        a.toggle(g(0), &y);
        b.toggle(g(0), &y);
        b.toggle(g(0), &x);
        assert_eq!(
            a.vector(g(0)),
            b.vector(g(0)),
            "order is not part of the answer"
        );

        a.toggle(g(0), &y);
        a.toggle(g(0), &y);
        assert_eq!(
            a.vector(g(0)),
            b.vector(g(0)),
            "toggling twice is toggling not at all"
        );

        // A version change is a difference, which is all the sweep needs.
        a.toggle(g(0), &y);
        a.toggle(g(0), &entry(22, 6));
        assert_ne!(a.vector(g(0)), b.vector(g(0)));
        assert_eq!(*b.vector(g(7)), [0u64; BUCKETS]);
        // Partitioning reaches the accumulators: another universe is another tree.
        a.toggle(GroupId::new(2, 0), &x);
        assert_ne!(a.vector(GroupId::new(2, 0)), a.vector(g(0)));
    }

    /// A range filter selects exactly one extent's pages and excludes other universes.
    #[test]
    fn a_filter_narrows_to_exactly_its_unit() {
        let addr = |u: u32, lba: u64| config::addr_of(u, lba);
        let f = Filter::Range {
            lo: addr(3, 10),
            hi: addr(3, 20),
        };
        assert!(
            f.keeps(addr(3, 10)) && f.keeps(addr(3, 19)),
            "the range is half open"
        );
        assert!(
            !f.keeps(addr(3, 9)) && !f.keeps(addr(3, 20)),
            "and it is exclusive at the top"
        );
        assert!(
            !f.keeps(addr(4, 15)) && !f.keeps(addr(2, 15)),
            "the same block in another universe is a different page"
        );

        assert!(
            Filter::All.keeps(addr(9, 9)),
            "the unfiltered cursor keeps everything"
        );
        let b = bucket_of(addr(1, 1));
        assert!(Filter::Bucket(b).keeps(addr(1, 1)));
        assert!(!Filter::Bucket(b ^ 1).keeps(addr(1, 1)));
    }

    /// A cursor sequence is six bits wide, so the sweep's own counter has to be folded
    /// into it rather than truncated by accident.
    #[test]
    fn a_chunk_sequence_cycles() {
        for seq in [0u8, 1, 62, 63] {
            assert_eq!(Seq::wrap(seq).get(), seq);
        }
        assert_eq!(Seq::wrap(64).get(), 0);
        assert_eq!(Seq::wrap(u8::MAX).get(), 63);
    }

    /// Cursor atomicity: a page overwritten ahead of the walk still reports its value at
    /// open, and its slot is not reused until the cursor is gone.
    #[test]
    fn an_open_cursor_sees_the_group_it_opened_on() {
        let n = TUPLES + 30;
        let mut entries: Vec<Entry> = (0..n).map(|i| entry(i as u64 + 1, 1)).collect();
        let gof: &Groups = &|_| GroupId::default();
        let now = Instant::now();
        let mut free = Vec::new();
        let mut s = Snaps::default();

        let id = s
            .start(0, false, GroupId::default(), Filter::All, now)
            .unwrap();
        let (first, done) = s.next(id, Some(0), None, &entries, gof, now).unwrap();
        assert_eq!((first.len(), done), (TUPLES, false));
        // A retry re-answers the same chunk; skipping would silently under-report.
        assert_eq!(
            s.next(id, Some(0), None, &entries, gof, now).unwrap().0,
            first
        );

        // Free an entry the walk has not reached yet, the way `Slab::set` does.
        let victim = n - 1;
        s.retain(&entries[victim]);
        entries[victim] = Entry::default();
        s.park(&mut free, victim as u32);
        assert!(
            free.is_empty(),
            "a slot freed under a cursor is deferred, not reused"
        );

        let mut seen: Vec<u64> = first.iter().map(|t| t.0).collect();
        let mut seq = 1;
        loop {
            let (chunk, done) = s.next(id, Some(seq), None, &entries, gof, now).unwrap();
            seen.extend(chunk.iter().map(|t| t.0));
            if done {
                break;
            }
            seq += 1;
        }
        seen.sort_unstable();
        seen.dedup();
        assert_eq!(
            seen.len(),
            n,
            "every page live at open is in the stream exactly once"
        );

        s.stop(id, &mut free);
        assert_eq!(
            free,
            vec![victim as u32],
            "reclamation resumes when the last cursor goes"
        );
    }

    /// A cursor answers only the universe it was opened for; ids are guessable.
    #[test]
    fn a_cursor_answers_only_its_own_universe() {
        let entries = [entry(1, 1)];
        let gof: &Groups = &|_| GroupId::new(7, 0);
        let now = Instant::now();
        let mut s = Snaps::default();

        let id = s
            .start(0, false, GroupId::new(7, 0), Filter::All, now)
            .unwrap();
        assert!(s.next(id, Some(0), Some(7), &entries, gof, now).is_ok());
        assert!(s.next(id, Some(0), Some(8), &entries, gof, now).is_err());
        // A local sweep names no universe and is answered either way.
        assert!(s.next(id, Some(0), None, &entries, gof, now).is_ok());
    }

    /// A cursor whose peer stopped asking expires, and its id is not answerable after.
    #[test]
    fn a_cursor_nobody_advances_expires() {
        let entries = [entry(1, 1)];
        let gof: &Groups = &|_| GroupId::default();
        let now = Instant::now();
        let mut free = Vec::new();
        let mut s = Snaps::default();

        let id = s
            .start(0, false, GroupId::default(), Filter::All, now)
            .unwrap();
        s.retain(&entries[0]);
        s.park(&mut free, 0);
        s.expire(now + SNAP_TTL + Duration::from_secs(1), &mut free);
        assert_eq!(free, vec![0]);
        assert!(s.next(id, Some(0), None, &entries, gof, now).is_err());

        // The slab refuses more cursors than it will hold rather than growing.
        let ids: Vec<u32> = (0..MAX_SNAPS)
            .filter_map(|_| s.start(0, false, GroupId::default(), Filter::All, now))
            .collect();
        assert_eq!(ids.len(), MAX_SNAPS);
        assert!(
            s.start(0, false, GroupId::default(), Filter::All, now)
                .is_none()
        );
    }
}
