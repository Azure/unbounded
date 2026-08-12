//! Anti-entropy: per-group digests over this node's registers, the wire encoding of the
//! three group-addressed control ops, and the sweep that drives them.
//!
//! Join replay and periodic digest exchange are one mechanism: enumerate a group's
//! registers and hand the differences to `paxos::repair`.
//!
//! The stream carries no page bytes. A cursor emits `(addr, version, ballot)` and the
//! receiver pulls the page with an ordinary `GET`, as `LEARN` already does. So there is
//! no second data path, no CRC to re-verify in transit (the `GET` verifies at the
//! source), and a page that changes mid-stream arrives as a newer value rather than a
//! torn one.
//!
//! The "tree" is one flat level of 512 buckets: 512 digests are exactly one 4 KiB
//! trailer, and a root would only add a round trip.

use std::cell::{Cell, RefCell};
use std::collections::BTreeMap;
use std::time::{Duration, Instant};

use crate::alloc::{Allocator, GlobalAddr, Pressure, Status};
use crate::config::{self, Config, GroupId};
use crate::fabric::{self, Frame, Link, Op};
use crate::layout::{Class, Entry};
use crate::paxos::{Ballot, Paxos, Register};
use crate::runtime::{self, PoolBuf};

/// Maps a page address to its consensus group. The allocator's slab holds no config, so
/// every call that disturbs a register passes the mapping in.
pub(crate) type Groups<'a> = dyn Fn(u64) -> GroupId + 'a;

// -------------------------------------------------------------------------- shape

/// Buckets per group. 512 u64 is exactly one 4 KiB trailer, so a comparison is one
/// round trip and there is no reason for a second tree level.
pub(crate) const BUCKETS: usize = 512;

/// `(addr, version, ballot)` triples in one chunk. Two header slots, three per tuple.
const TUPLES: usize = (fabric::BLOCK / 8 - 2) / 3;

/// Concurrent cursors one slab will hold open. A cursor pins freed slots, so this also
/// bounds how much reclamation anti-entropy can defer.
const MAX_SNAPS: usize = 8;

/// Entries one `snap_next` may scan before answering with what it has, so a sparse
/// filter cannot walk the whole shard in one frame.
const SNAP_SCAN: u32 = 1 << 20;

/// Registers one slab holds against reclamation before failing its cursors. Retention
/// grows as `write_rate × cursor_lifetime`; past this, failing beats pinning the device.
const MAX_RETAINED: usize = 1 << 16;

/// A cursor nobody has advanced for this long is abandoned and its slots released.
const SNAP_TTL: Duration = Duration::from_secs(30);

/// The bucket a page's digest lands in. `config::slot_of` takes bits 0..14 of the same
/// hash and this takes 14..23, so a group's pages spread over all buckets instead of
/// piling into the few its slots select.
pub(crate) fn bucket_of(addr: u64) -> u16 {
    ((config::mix(addr) >> 14) & (BUCKETS as u64 - 1)) as u16
}

/// What a cursor emits, within the group it opened on. Applied as a predicate during
/// the walk, not an index, so a narrow filter still costs a full slab pass.
#[derive(Clone, Copy, PartialEq, Debug)]
pub(crate) enum Filter {
    All,
    /// One digest bucket: the anti-entropy comparison.
    Bucket(u16),
    /// A half-open span of the flat address space, `lo..hi`. One extent is one such
    /// span - universe and base concatenate into the address - so the unit a migration
    /// hands between zones needs no field of its own here.
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

/// The `n`th group in that flat sequence, universes in id order.
///
/// One cursor over one sequence rather than a cursor per universe: a node in two
/// universes then splits the same sweep budget between them in proportion to how many
/// groups each gave it, which is the share of the data it holds for each.
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

/// The leaf tuple. Not cryptographic: a digest match is a hint that the backlog is
/// small, never proof of equality.
fn leaf(addr: u64, version: u64, ballot: u64, crc: u32) -> u64 {
    config::mix(addr ^ config::mix(version ^ config::mix(ballot ^ crc as u64)))
}

// -------------------------------------------------------------------------- digests

/// Per-group XOR accumulators over the registers this node owns.
///
/// XOR makes an update two calls with nothing to recompute and no ordering to preserve.
/// Keyed by `(group, class)` — one `Digests` per slab — rather than by shard: the shard
/// a page lands on depends on the local core count, so two nodes would not agree on
/// which tree they were comparing, whereas the class is in the frame.
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

    /// A group we hold nothing for reads as all zeroes — the digest of the empty set —
    /// so it compares equal to a peer that also holds nothing.
    pub(crate) fn vector(&self, group: GroupId) -> Box<[u64; BUCKETS]> {
        self.groups
            .get(&group)
            .cloned()
            .unwrap_or_else(|| Box::new([0; BUCKETS]))
    }

    /// Groups this node holds registers for. A group it never held one for was never
    /// inserted, so the set of groups it has been moved out of but is still carrying is
    /// exactly this list minus the catalog's — a map walk rather than a slab scan.
    pub(crate) fn held(&self) -> Vec<GroupId> {
        self.groups.keys().copied().collect()
    }

    /// Drop a group whose accumulator has gone back to zero. `toggle` never prunes —
    /// noticing on the hot path would cost a scan of the vector — so the shed that
    /// emptied the group says so here, and `held` stops offering it.
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

// -------------------------------------------------------------------------- cursors

/// A register as the cursor emits it. No page bytes: the receiver pulls those with an
/// ordinary `GET`, so a page that changed since the cursor opened arrives newer rather
/// than torn.
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

/// A cursor id, self-describing so `SNAPNEXT` needs nothing but the id to find the slab
/// that owns it: `generation | core | class | slot`.
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
    /// Next local entry index. Monotone, so the walk is one slab pass however many
    /// chunks it takes.
    cursor: u32,
    /// Where this cursor's view of `retained` begins: everything freed after it opened.
    drained: usize,
    /// Last chunk answered, so a retried `SNAPNEXT` repeats rather than skips.
    seq: u8,
    last: Vec<Tuple>,
    last_done: bool,
    touched: Instant,
}

/// The open cursors on one slab, plus the reclamation they hold back.
///
/// Deferral is a cursor's atomicity: a slot freed under an open cursor is neither
/// reused nor forgotten, so a page overwritten mid-walk is still emitted with the value
/// it had when the cursor opened. Without it, a page whose slot got recycled ahead of
/// the cursor would vanish from the stream entirely.
#[derive(Default)]
pub(crate) struct Snaps {
    open: [Option<Snap>; MAX_SNAPS],
    live: usize,
    era: u32,
    /// Local slot ids freed while a cursor was open; returned to the free list when the
    /// last one closes.
    deferred: Vec<u32>,
    /// Registers of those freed entries, in release order. A cursor emits the tail of
    /// this list that appeared after it opened.
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

    /// A freed slot is parked while a cursor is open, freed otherwise. Void cursors hold
    /// nothing back.
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

    /// Next chunk. A repeated `seq` re-answers the previous chunk, anything else
    /// advances; `None` is the local caller, which has no wire to lose a reply on.
    ///
    /// `universe` is the namespace the request arrived on, `None` for a local caller. A
    /// cursor id is a small integer a peer could guess, so the universe it was opened in
    /// is checked against the one asking: partitioning has to hold for a cursor exactly
    /// as it holds for a page.
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
        // Retained values drain last: they are older than whatever the live table now
        // holds at the same address, and the receiver merges apply-if-newer.
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

    /// Abandon cursors nobody has advanced. A peer that died mid-stream must not hold
    /// reclamation forever; the TTL is all that stands between a lost reply and a
    /// pinned device.
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

// -------------------------------------------------------------------------- the wire

/// Frame conventions for the three ops below. They name a *group*, not a page, so
/// `offset` means something different here than in every other op and `server::addr_of`
/// is deliberately not on their path.
///
/// A group index alone is not a group: the universe is the namespace the frame arrived
/// on, so the target rebuilds the [`GroupId`] from its own side of the link and a peer
/// cannot name a group in a universe it was never given a namespace for.
///
/// All three encode with `Frame::huge = false` whatever class they ask about: the huge
/// frame shape has ten fewer offset bits and these need the room. MERKLE and SNAPOPEN
/// carry the class in `imm` bit 0; SNAPNEXT carries it inside the cursor id.
///
///   MERKLE    offset = group                       -> 512 digests, the whole trailer
///   SNAPOPEN  offset = group << 10 | one << 9 | b  -> slot 0: cursor id
///             `one` set means filter to bucket `b`
///   SNAPNEXT  offset = cursor id << 6 | chunk seq  -> slot 0: count, slot 1: done
///                                                     then 3 slots per tuple
pub(crate) fn merkle_frame(group: u32, huge: bool) -> Frame {
    let mut f = Frame::raw(Op::Merkle, false, group as u64);
    f.imm = huge as u8;
    f
}

pub(crate) fn snap_open_frame(group: u32, huge: bool, bucket: Option<u16>) -> Frame {
    let b = bucket.unwrap_or(0) as u64 & (BUCKETS as u64 - 1);
    let off = (group as u64) << 10 | (bucket.is_some() as u64) << 9 | b;
    let mut f = Frame::raw(Op::SnapOpen, false, off);
    f.imm = huge as u8;
    f
}

pub(crate) fn snap_next_frame(id: u32, seq: u8) -> Frame {
    Frame::raw(Op::SnapNext, false, (id as u64) << 6 | (seq & 0x3f) as u64)
}

/// The request side of the two cursor ops, as the target reads them back.
pub(crate) fn snap_open_parts(f: &Frame) -> (u32, bool, Option<u16>) {
    let bucket = (f.offset >> 9 & 1 == 1).then(|| (f.offset & (BUCKETS as u64 - 1)) as u16);
    ((f.offset >> 10) as u32, f.imm & 1 == 1, bucket)
}

pub(crate) fn snap_next_parts(f: &Frame) -> (u32, u8) {
    ((f.offset >> 6) as u32, (f.offset & 0x3f) as u8)
}

/// Digest vector into a trailer and back. Exactly full, so there is no count.
pub(crate) fn put_digests(t: &mut [u8], v: &[u64; BUCKETS]) {
    for (i, d) in v.iter().enumerate() {
        fabric::put(t, i, *d);
    }
}

pub(crate) fn get_digests(t: &[u8]) -> Box<[u64; BUCKETS]> {
    let mut v = Box::new([0u64; BUCKETS]);
    for (i, d) in v.iter_mut().enumerate() {
        *d = fabric::get(t, i);
    }
    v
}

pub(crate) fn put_tuples(t: &mut [u8], tuples: &[(u64, Register)], done: bool) {
    fabric::put(t, 0, tuples.len() as u64);
    fabric::put(t, 1, done as u64);
    for (i, (addr, r)) in tuples.iter().enumerate() {
        fabric::put(t, 2 + i * 3, *addr);
        fabric::put(t, 3 + i * 3, r.version);
        fabric::put(t, 4 + i * 3, r.ballot.raw() as u64);
    }
}

pub(crate) fn get_tuples(t: &[u8], out: &mut BTreeMap<u64, Register>) -> bool {
    let n = (fabric::get(t, 0) as usize).min(TUPLES);
    for i in 0..n {
        let addr = fabric::get(t, 2 + i * 3);
        let version = fabric::get(t, 3 + i * 3);
        let ballot = Ballot::from_raw(fabric::get(t, 4 + i * 3) as u32);
        out.insert(addr, Register { version, ballot });
    }
    fabric::get(t, 1) != 0
}

// -------------------------------------------------------------------------- the sweep

/// Differing buckets one sweep reconciles before moving on. What it skips is left for
/// the next pass; convergence does not depend on any single sweep finishing.
const BUCKETS_PER_JOB: usize = 8;

/// Repairs one sweep will issue. A repair is a full prepare round against the whole
/// group, so this keeps a large divergence from starving the write path.
const REPAIRS_PER_JOB: usize = 64;

/// Repair budget while replaying, and the per-call push budget in `push_extent`. A
/// joining node has the whole group to fetch and, since it neither proposes nor accepts
/// until it is caught up, no client write path of its own to protect. The prepare round
/// per address is not avoidable: a register held by one member alone was not
/// necessarily chosen, and copying it here would give it a second acceptor.
///
/// The one budget here the operator sets, because it is the rate a member replacement
/// runs at and the length of the window a group spends two of three, and the rate an
/// extent is handed to another zone at: `Policy::repairs_per_replay`.
fn repairs_per_replay(cfg: &Config) -> usize {
    cfg.policy.repairs_per_replay as usize
}

/// Tuples one side of a bucket comparison will buffer. A bucket past this is a group
/// too large for the fanout; it is skipped and counted rather than allowed to grow a
/// map without limit.
const MAX_BUCKET: usize = 1 << 16;

/// Shortest gap between sweeps on one core.
const INTERVAL: Duration = Duration::from_secs(1);

/// Registers one sweep will offer back before moving on. Each costs a `GETMETA` at all
/// three of the group's new members, so this is the same kind of budget as
/// `REPAIRS_PER_JOB` and is set the same way.
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

#[derive(Default)]
struct Core {
    /// One sweep at a time per core: a job still running declines the next tick, which
    /// paces `tick` without a queue.
    busy: Cell<bool>,
    last: Cell<Option<Instant>>,
    /// Round-robin over the groups this core owns, so no group is starved by a noisy
    /// neighbour and a long divergence is amortised over many passes.
    next: Cell<u32>,
    stats: RefCell<Stats>,
}

pub struct Heal {
    paxos: &'static Paxos,
    cores: Box<[Core]>,
}

// SAFETY: as `Paxos`. Every cell is touched only by the core that owns it, which is
// the core `tick` ran on and the core the spawned job stays on.
unsafe impl Sync for Heal {}

pub fn open(paxos: &'static Paxos, cores: usize) -> &'static Heal {
    let cores = (0..cores).map(|_| Core::default()).collect();
    Box::leak(Box::new(Heal { paxos, cores }))
}

impl Heal {
    fn alloc(&self) -> &'static Allocator {
        self.paxos.alloc()
    }

    /// This core's counters; the exporter publishes a row per core and sums them.
    pub fn local_stats(&self) -> Stats {
        *self.cores[runtime::core()].stats.borrow()
    }

    /// The two halves of a member replacement still in flight on this core: groups being
    /// replayed into, and groups still holding registers they have been moved out of.
    ///
    /// The control plane's only completion signal. It replaces one node at a time and
    /// must not replace the next until this reads zero across the zone: a second node in
    /// flight puts more groups at two of three at once, and a node that has not finished
    /// shedding has not returned the space the next replacement needs.
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
        f(&mut self.cores[runtime::core()].stats.borrow_mut());
    }

    /// Called from `Handler::tick`, which is synchronous, so the sweep is spawned.
    /// Declines under rate pressure: the device's budget is the write path's budget too.
    ///
    /// Free-space pressure does not decline here, though it once did. Shedding is where
    /// space comes back from, and the node that is full is the node that needs it most,
    /// so the sweep runs and skips its anti-entropy half instead.
    pub fn tick(&'static self, now: Instant) {
        let c = &self.cores[runtime::core()];
        if c.busy.get()
            || c.last
                .get()
                .is_some_and(|t| now.duration_since(t) < INTERVAL)
        {
            return;
        }
        if self.alloc().store_pressed() {
            return;
        }
        c.last.set(Some(now));
        c.busy.set(true);
        if !runtime::spawn(async move {
            let _ = self.sweep().await;
            self.cores[runtime::core()].busy.set(false);
        }) {
            c.busy.set(false);
        }
    }

    /// One group per sweep, both classes, one peer. Everything past the first frame is
    /// conditional on a digest that did not match, so a converged cluster pays one
    /// MERKLE per group and class per interval and nothing else.
    async fn sweep(&'static self) -> Result<(), Status> {
        let core = runtime::core();
        let c = &self.cores[core];
        let cfg = self.alloc().config();
        let groups = total_groups(cfg);
        if groups == 0 {
            return Ok(());
        }

        // Shedding comes first and runs regardless of free space: under pressure it is
        // the only half worth running, and everything below competes for the space it
        // is trying to return.
        for huge in [false, true] {
            if self.serves(huge) {
                self.shed(huge).await;
            }
        }
        if self.alloc().pressure() != Pressure::Normal {
            return Ok(());
        }

        // Pick a group whose paxos core is this one, so `repair` and the allocator shard
        // are both local wherever the shard count reached the core count.
        //
        // A group already replaying goes first. Round-robin alone gives a joining group
        // one budget every `groups / cores` intervals, which is how long a replacement
        // would take per group; jumping the queue for it costs the settled groups a
        // digest exchange they would only have found equal.
        let cores = self.cores.len();
        let g;
        match self
            .paxos
            .replaying_here()
            .into_iter()
            .find(|&g| self.paxos.members(g).is_some())
        {
            Some(r) => g = r,
            None => {
                let start = c.next.get();
                let mut n = start;
                loop {
                    match group_at(cfg, n) {
                        Some(cand)
                            if cand.index() as usize % cores == core
                                && self.paxos.members(cand).is_some() =>
                        {
                            g = cand;
                            break;
                        }
                        _ => {}
                    }
                    n = (n + 1) % groups;
                    if n == start {
                        return Ok(());
                    }
                }
                c.next.set((n + 1) % groups);
            }
        }

        self.stat(|s| s.sweeps += 1);
        // Replay is sticky: one repaired bucket already makes our side non-empty while
        // the rest of the group is still missing, so re-detecting it each sweep would
        // clear the flag after one pass. Only a comparison with nothing left to repair
        // ends it.
        let was = self.paxos.replaying(g).await;
        let mut replaying = false;
        let mut checked = true;
        for huge in [false, true] {
            if !self.serves(huge) {
                continue;
            }
            match self.compare(cfg, g, huge, was).await {
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
            // Leaving a replay means recovering the promise that went with the registers
            // first. Until that lands we stay out; the next sweep retries.
            self.stat(|s| s.failed += 1);
        }
        self.hand_over(cfg, g).await;
        Ok(())
    }

    /// The source side of an extent migration. The destination cannot pull: it holds
    /// neither this zone's slot table nor its catalog, so the zone handing the extent
    /// over is the one that pushes. Every member pushes its own copy and the stream is
    /// apply-if-newer, so the duplicate streams need no ordering against each other.
    ///
    /// The seal comes first and freezes the extent here, so a page pushed after it
    /// cannot be overtaken by a write we accept.
    ///
    /// Pushing repeats until the control plane flips `zone` and clears `next_zone`, the
    /// only completion signal there is. Repetition is harmless and is what makes a
    /// destination that was down for the first pass converge anyway.
    async fn hand_over(&'static self, cfg: &Config, group: GroupId) {
        // Only this group's own universe: a group holds registers for nothing else, so
        // the other universes' extents are somebody else's sweep.
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
                // Push on the next tick: a seal only just chosen may not have reached
                // the members whose copies we are about to walk.
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

    /// Push this group's registers for one extent to the zone taking it over, at most
    /// the config's replay budget per call.
    async fn push_extent(
        &'static self,
        group: GroupId,
        huge: bool,
        filter: Filter,
        zone: u32,
    ) -> Result<(), Status> {
        let id = self.alloc().snap_open(group, huge, filter).await?;
        let mut sent = 0usize;
        loop {
            let (tuples, done) = self.alloc().snap_next(id, None, None).await?;
            for (addr, r) in tuples {
                let _ = self.paxos.push(GlobalAddr(addr), r, zone).await;
                sent += 1;
            }
            if done || sent >= repairs_per_replay(self.alloc().config()) {
                break;
            }
        }
        self.alloc().snap_release(id).await;
        Ok(())
    }

    /// Whether this node has a slab of the class at all: no slots, no cursor to open.
    ///
    /// The device's geometry rather than the configuration's page count, because the
    /// geometry is what was actually formatted, and a node draining after the catalog
    /// stopped naming it still has to open a cursor over everything it holds.
    fn serves(&self, huge: bool) -> bool {
        let c = if huge { Class::Huge } else { Class::Small };
        self.alloc().geometry().slots(c) > 0
    }

    // ---------------------------------------------------------------------- the shed

    /// Give back the registers of groups this node is no longer in the catalog for.
    ///
    /// Replacing a node is how a zone's membership moves: the node joining takes the
    /// departing node's groups and replays them, and the node leaving is left holding
    /// every register it ever accepted, because nothing on the write path revisits a
    /// page once it is placed. Without this a node only ever grows and a decommission
    /// frees nothing.
    ///
    /// The digest map names the work exactly. A group we hold no register for was never
    /// inserted into it, so the groups it lists minus the ones the catalog still names
    /// us in is the whole of the set, found without a slab scan.
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
            // One group that will not drain must not block the others; the next sweep
            // comes back to it.
            if self.drain(g, huge, &mut budget).await.is_err() {
                self.stat(|s| s.failed += 1);
            }
        }
    }

    /// Walk one orphaned group and forget every register its new members can be shown to
    /// hold. A register nobody confirmed stays, so the degraded window between the
    /// catalog naming a new member and that member catching up costs availability, never
    /// the value.
    async fn drain(
        &'static self,
        group: GroupId,
        huge: bool,
        budget: &mut usize,
    ) -> Result<(), Status> {
        let id = self.alloc().snap_open(group, huge, Filter::All).await?;
        let mut walked = false;
        while *budget > 0 {
            let (tuples, done) = self.alloc().snap_next(id, None, None).await?;
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
        self.alloc().snap_release(id).await;
        // Only a full pass proves the group is empty, and `forget` re-checks anyway.
        if walked {
            self.alloc().forget_group(group, huge).await;
        }
        Ok(())
    }

    /// Returns whether this group is still being replayed. `was` is the flag we came in
    /// with, which only a comparison with nothing left to repair may clear.
    async fn compare(
        &'static self,
        cfg: &Config,
        group: GroupId,
        huge: bool,
        was: bool,
    ) -> Result<bool, Status> {
        let m = self.paxos.members(group).ok_or(Status::Unmapped)?;
        let me = self.paxos.self_index(&m).ok_or(Status::Unmapped)?;
        // The partner offset alternates with group parity, spreading a node's own
        // comparisons over both peers; within one group the three members between them
        // walk all three edges of the triangle.
        let from = ((me as u32 + 1 + group.index() % 2) % 3) as u8;
        let link = self
            .paxos
            .link_of(group.universe(), m[from as usize])
            .ok_or(Status::Io)?;

        let mine = self.alloc().digests(group, huge).await;
        let t = PoolBuf::alloc(fabric::BLOCK).await;
        link.send(merkle_frame(group.index(), huge), t.buf())
            .await
            .map_err(Status::from_wire)?;
        let theirs = get_digests(&t);

        // Our whole side of the group empty against a peer that has data is a node
        // joining, not a divergence. The work is the same — every bucket, repaired —
        // but the budgets are not, and this node refuses accepts for the group until
        // the digests agree.
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
            // Nothing either side holds that the other does not: the only thing that
            // ends a replay.
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

    /// Enumerate one bucket on both sides and repair every address the two do not agree
    /// on, in either direction. The registers are only a difference test — `repair`
    /// re-derives the truth with a prepare round, so a cursor that raced a write costs
    /// an extra repair and never a wrong one.
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
            // Addresses we hold and they do not; the loop above never saw these.
            if !theirs.contains_key(addr) {
                *budget -= 1;
                self.repair(cfg, GlobalAddr(*addr)).await;
            }
        }
        Ok(())
    }

    async fn repair(&'static self, cfg: &Config, addr: GlobalAddr) {
        // A page whose extent left the config between the cursor and here is not a
        // divergence, it is a page that no longer exists.
        if cfg.extent_at(addr.0).is_none() {
            return;
        }
        match self.paxos.repair(addr).await {
            Ok(_) => self.stat(|s| s.repairs += 1),
            Err(_) => self.stat(|s| s.failed += 1),
        }
    }

    /// A cursor abandoned here — the peer went away mid-walk, or the bucket turned out
    /// larger than we will buffer — is not released; only the TTL in `Snaps::expire`
    /// reclaims it. A finished cursor is released by the server on the last chunk.
    async fn remote_bucket(
        &'static self,
        link: &Link,
        group: GroupId,
        huge: bool,
        bucket: u16,
    ) -> Result<Option<BTreeMap<u64, Register>>, Status> {
        let t = PoolBuf::alloc(fabric::BLOCK).await;
        let f = snap_open_frame(group.index(), huge, Some(bucket));
        link.send(f, t.buf()).await.map_err(Status::from_wire)?;
        let id = fabric::get(&t, 0) as u32;

        let mut out = BTreeMap::new();
        for seq in 0..=u8::MAX {
            let t = PoolBuf::alloc(fabric::BLOCK).await;
            let f = snap_next_frame(id, seq);
            link.send(f, t.buf()).await.map_err(Status::from_wire)?;
            if get_tuples(&t, &mut out) {
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
        let id = self
            .alloc()
            .snap_open(group, huge, Filter::Bucket(bucket))
            .await?;
        let mut out = BTreeMap::new();
        let mut over = false;
        loop {
            let (tuples, done) = self.alloc().snap_next(id, None, None).await?;
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
        self.alloc().snap_release(id).await;
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

    /// Putting a tuple in and taking it out are the same operation, so an entry that
    /// changed and changed back leaves no trace and arrival order cannot matter.
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

        // A version change is a difference, which is the only property the sweep needs.
        a.toggle(g(0), &y);
        a.toggle(g(0), &entry(22, 6));
        assert_ne!(a.vector(g(0)), b.vector(g(0)));
        // And a group nobody has touched is the digest of the empty set.
        assert_eq!(*b.vector(g(7)), [0u64; BUCKETS]);
        // Nor has the same index in another universe: partitioning reaches the
        // accumulators, so two universes never compare trees with each other.
        a.toggle(GroupId::new(2, 0), &x);
        assert_ne!(a.vector(GroupId::new(2, 0)), a.vector(g(0)));
    }

    /// A migration names an extent by the address range it covers, which every node can
    /// evaluate for itself: the filter lets one zone walk exactly the pages it is
    /// handing over, without the destination naming a table it does not hold. Universe
    /// and block index concatenate, so one span is one comparison and a neighbouring
    /// universe cannot fall inside it.
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

    /// The three group-addressed frames survive the wire, including the `offset` and
    /// `imm` fields they borrow for meanings no other opcode gives them.
    #[test]
    fn group_frames_survive_encoding() {
        let round = |f: Frame| {
            let e = f.encode();
            Frame::decode(e, fabric::BLOCK).unwrap().0
        };
        let m = round(merkle_frame(1234, true));
        assert_eq!((m.offset, m.imm & 1 == 1), (1234, true));

        for bucket in [None, Some(0), Some(511u16)] {
            let f = round(snap_open_frame(300, false, bucket));
            assert_eq!(snap_open_parts(&f), (300, false, bucket));
        }
        // Six bits of sequence: enough that no two consecutive chunks collide, which is
        // all a retry needs to tell apart.
        for seq in [0u8, 1, 62, 63] {
            let f = round(snap_next_frame(0x0dedbeef, seq));
            assert_eq!(snap_next_parts(&f), (0x0dedbeef, seq));
        }
        assert_eq!(snap_next_parts(&round(snap_next_frame(7, 64))).1, 0);
    }

    /// Cursor atomicity: a page overwritten ahead of the walk is still reported with
    /// the value it had when the cursor opened, and its slot is not handed out until
    /// the cursor is gone. Without that, a page whose slot got recycled ahead of the
    /// cursor would vanish from the stream and read as agreement.
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
        // A retry is the same chunk, not the next one. A cursor that skipped would
        // under-report a difference, which is silent and so the worst failure this
        // stream has.
        assert_eq!(
            s.next(id, Some(0), None, &entries, gof, now).unwrap().0,
            first
        );

        // Now free an entry the walk has not reached yet, the way `Slab::set` does.
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

    /// A cursor id is a small integer, so guessing one is trivial. What stops a peer in
    /// one universe reading another's pages through a guessed id is that the cursor
    /// remembers the universe it was opened for and refuses every other.
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

    /// A cursor whose peer stopped asking must not pin the device forever, and the id
    /// it held is not answerable afterwards.
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

        // And the slab refuses more cursors than it will hold rather than growing.
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
