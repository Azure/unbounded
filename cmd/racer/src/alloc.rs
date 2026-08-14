//! The allocator: page placement, IO, and the cross-core hops into shard state.
//!
//! Durable formats live in `layout.rs`, the decisions in `shard.rs`. One `Shard` per
//! worker core, no locks, no atomics, no background threads. A page is placed by a
//! free-list pop, written out of place, and its metadata rides a group-committed 4 KiB
//! mblock write. `Small` (4 KiB) carries a CRC in its mblock entry, verified on every
//! read; a page that does not match is never served. `Huge` (4 MiB) carries none, so
//! ordering is its only defence against a torn or lost data write. Both write data
//! before the entry naming it; `finish_small` gives the small class's own reason.
//! Policy lives elsewhere: anti-entropy in `heal.rs`, CASPaxos in `paxos.rs`.

mod shard;

use std::cell::RefCell;
use std::future::Future;
use std::pin::Pin;
use std::sync::OnceLock;
use std::task::{Context, Poll, Waker};
use std::time::Instant;

use crate::cache::Cache;
use crate::config::{Config, GroupId, Kind};
use crate::heal::{self, Tuple};
use crate::layout::{self, Class, Geometry, MBLOCK};
use crate::paxos::{Ballot, Register};
use crate::runtime::{self, Buf, CoreId, Disk, Durability, PoolBuf};
use crate::server::{self, Server};

use shard::Ticket;
use shard::{Act, Lookup, Maps, Shape, Shard, Staged};
pub use shard::{GlobalAddr, Pressure, Status};

/// Bind the two config lookups a shard mutation needs as `$m`. A macro because the
/// bindings borrow the config and so must land in the caller's own scope.
macro_rules! maps {
    ($cfg:expr, $m:ident) => {
        let cfg = $cfg;
        let gof = |a: u64| cfg.group(a);
        let xof = |a: u64| {
            cfg.extent_at(a)
                .map(|e| (e.id, e.tombstone_epoch as u32, e.kind))
        };
        // A universe with peers is a full publication, so an address none of its extents
        // cover has been taken away. A universe without them is the bootstrap shape the
        // agent publishes before its attachments land, which names no extents at all and
        // must not be read as having retired every one of them.
        let rof = |a: u64| {
            cfg.universe(GlobalAddr(a).universe())
                .is_some_and(|u| !u.peers.is_empty())
        };
        let $m = Maps {
            gof: &gof,
            xof: &xof,
            rof: &rof,
        };
    };
}

/// A durable page whose entry is not installed yet: a slot held, bytes on the device, and
/// nothing in the index pointing at either.
///
/// It carries the address it was opened for, so the only thing that can be committed or
/// given back is the thing that was reserved. Passing the address again would let a caller
/// name a different one, and the failure is silent both ways: the checksum of a 4 KiB page
/// is seeded with its address, and a 4 MiB page has no checksum to disagree.
///
/// Holding one is an obligation. It cannot be copied, so it cannot be settled twice, and
/// it is `#[must_use]` so dropping one is at least deliberate. Rust cannot make a
/// destructor await, so the slot behind a dropped token is leaked until restart; the
/// index model counts that as a leak rather than pretending it cannot happen.
#[must_use = "a pending page holds a slot until it is committed or given back"]
pub struct Pending {
    addr: GlobalAddr,
    class: Class,
    ticket: Ticket,
    crc: u32,
}

/// A 4 MiB data slot the cache is holding for as long as the slab does not want it back.
///
/// It carries the slot it came from and not just where that slot is. The offset can be
/// derived from the slot, but going back the other way is a lookup that can fail, and a
/// conversion that can fail on the way home is a loan that can be dropped on the floor
/// while the shard that made it still counts the slot lent.
///
/// Not `Copy`, so there is one of these per lent slot and [`Allocator::reclaim`] spends
/// it. Rust cannot make a destructor reach a shard that the caller may already have open,
/// so a dropped loan still leaks its slot until restart; what the type buys is that the
/// leak has to be written rather than fallen into.
#[must_use = "a lent slot stays lent until the loan is handed back"]
pub struct Loan {
    slot: u32,
    off: u64,
}

impl Loan {
    /// Where the borrowed media is.
    pub fn offset(&self) -> u64 {
        self.off
    }

    /// A loan against no shard, for tests that only care that one is held.
    #[cfg(test)]
    pub(crate) fn for_test(off: u64) -> Loan {
        Loan { slot: 0, off }
    }
}

/// DRAM cost of one resident small page: mblock entry plus its share of the index.
/// `config::validate` refuses a working set over `policy.max_index_bytes`, avoiding OOM.
pub const INDEX_BYTES_PER_PAGE: u64 = 52;

/// A slab lends only while more than `1/LEND_CEILING` of its stripe is genuinely free,
/// and recalls loans below `1/LEND_FLOOR`. Between the two it neither lends nor reclaims.
pub(super) const LEND_CEILING: u64 = 4;
const LEND_FLOOR: u64 = 8;
/// Loans recalled per reservation: ahead of one allocating core, less than a full refill.
const LEND_BATCH: u64 = 8;

/// DRAM cost of one OCC read record: map entry, share of the table, place in pool order.
const OCC_BYTES_PER_RECORD: u64 = 48;

// ------------------------------------------------------------------------- allocator

/// One worker core's state; the decisions live in `Shard` (`shard.rs`).
struct Core {
    shard: Shard,
    /// Woken by every flush completion on this core; waiters re-check their own condition.
    waiters: Vec<Waker>,
    /// 4 KiB mblock serialisation buffer per class, pre-held by `tick` so flush never awaits.
    staging: [Option<PoolBuf>; 2],
    /// Committers parked behind a flush, by class.
    commit_parks: [u64; 2],
    /// Time metadata flushes were in flight, by class.
    flush_busy_us: [u64; 2],
    flush_started: [Option<Instant>; 2],
    /// 4 MiB pages being reassembled from the pieces a transport split them into.
    parts: Vec<Parts>,
}

/// One 4 MiB page arriving in pieces. The first piece reserves the slot so the rest
/// land where the page will live. `have` tracks durable blocks: the class carries no
/// checksum, so a hole must be caught before the entry naming the page is written.
struct Parts {
    addr: GlobalAddr,
    /// The command the pieces belong to. Two commands for one address can be in flight
    /// at once (member accept, non-member proposal); their bytes must never mix.
    key: PartsKey,
    ticket: Ticket,
    /// Pieces whose write is in flight; an assembly is evicted only at zero, else its slot
    /// would go to another page mid-write.
    busy: u32,
    have: [u64; (layout::HUGE_PAGE / layout::SMALL_PAGE / 64) as usize],
    blocks: u32,
}

/// Guard, ballot and proposer index: what makes two pieces parts of one page.
type PartsKey = (u64, u32, u8);

/// Assemblies one core will hold at once. A transfer that stops halfway holds its slot
/// until the eighth assembly after it needs the room: bounded reclamation, no timer.
const HUGE_PARTS: usize = 8;

/// Blocks in a 4 MiB page.
const HUGE_BLOCKS: u32 = (layout::HUGE_PAGE / layout::SMALL_PAGE) as u32;

pub struct Allocator {
    disk: Disk,
    geo: Geometry,
    cores: usize,
    /// Consensus side state as the superblock held it at startup: `promised_term` per
    /// group and the seal table. `paxos` takes it from here and owns it thereafter.
    boot: layout::Consensus,
    /// Metadata blocks lost at startup, surfaced as a health metric.
    pub quarantined: usize,
    /// The cache, once it exists. Only so the allocator can call in a loan of a free
    /// 4 MiB slot without waiting for the cache to notice; see [`Allocator::top_up_in`].
    cache: OnceLock<&'static Cache>,
}

/// One worker's share of the allocator, living in that worker's [`server::CoreState`].
///
/// A shard is not a partition of one table but a table of its own: it owns a stripe of
/// the mblocks, allocates only from it, and holds the registers of the groups whose id
/// maps to this core. So there is nothing here another core has any business reading,
/// and the borrow that proves it is the one the worker takes on its own row.
pub(crate) struct Row(RefCell<Core>);

/// One class's group-commit counters on this core.
#[derive(Clone, Copy, Default)]
pub struct ClassStats {
    pub commits: u64,
    pub flushes: u64,
    pub flush_batch: u64,
    pub parks: u64,
    pub busy_us: u64,
    /// Rows the sweep reclaimed because their extent's epoch moved past them.
    pub swept_epoch: u64,
    /// Rows the sweep reclaimed because no extent covers their address any more.
    pub swept_uncovered: u64,
}

/// Allocator counters. One set per core; the exporter sums them.
#[derive(Clone, Copy, Default)]
pub struct Stats {
    pub per: [ClassStats; 2],
}

// SAFETY: a row crosses to its worker once, before that worker takes traffic, and what
// makes `Core` unsendable are the staging buffers: a `PoolBuf` is an index into one ring's
// registered set and means nothing on another core. `Row::new` is the only constructor
// and it holds no buffer, and the only code that can put one there is `here` or `at`,
// which is the owning worker running a transaction. So the value that travels holds
// nothing that belongs to where it is going.
unsafe impl Send for Row {}

impl Row {
    fn new(shard: Shard) -> Row {
        Row(RefCell::new(Core {
            shard,
            waiters: Vec::with_capacity(64),
            staging: [None, None],
            commit_parks: [0; 2],
            flush_busy_us: [0; 2],
            flush_started: [None, None],
            parts: Vec::new(),
        }))
    }
}

/// This core's share.
fn here<T>(f: impl FnOnce(&mut Core) -> T) -> T {
    runtime::here::<Server, T>(|ctx| f(&mut ctx.state().alloc.0.borrow_mut()))
}

/// The decision-making half of it, which is most of what callers want.
fn shard<T>(f: impl FnOnce(&mut Shard) -> T) -> T {
    here(|c| f(&mut c.shard))
}

/// `core`'s share, as a transaction that core runs.
///
/// Synchronous by construction, which is what makes it a transaction: a reservation, a
/// lookup or a register read is one visit to the owning worker, resolved inside the drain
/// that delivered it, rather than a task parked there waiting to be polled again.
fn find_parts(c: &Core, addr: GlobalAddr, key: PartsKey) -> Option<usize> {
    c.parts.iter().position(|p| p.addr == addr && p.key == key)
}

fn at<T, F>(core: CoreId, f: F) -> impl Future<Output = T>
where
    F: FnOnce(&mut Core) -> T + Send + 'static,
    T: Send + 'static,
{
    runtime::with_core::<Server, _, _>(core, move |ctx| f(&mut ctx.state().alloc.0.borrow_mut()))
}

/// Cores that participate in a class's index sharding. Mblocks are striped `id % cores`
/// and a core allocates only from its own stripe, so capping the width at the mblock
/// count keeps every owning core able to allocate. One mblock covers 504 MiB, so a
/// modest huge slab has fewer mblocks than workers.
fn shards_for(cores: usize, geo: &Geometry, class: Class) -> usize {
    cores.min(geo.mblocks(class).max(1) as usize)
}

/// This core's share of the OCC pool. Per core because a record is only touched by its
/// owning core and a shared pool would need a hot-path lock. Cost: a dropped record
/// turns a success into a conflict, which is a retry, never a wrong answer.
fn occ_per_core(bytes: u64, cores: usize) -> usize {
    (bytes / OCC_BYTES_PER_RECORD / cores.max(1) as u64).max(1) as usize
}

/// The shape the device's geometry implies; `shard::model` supplies its own numbers.
fn shape_of(geo: &Geometry, cfg: &Config, cores: usize) -> Shape {
    Shape {
        cores: cores as u32,
        k: [Class::Small.k(), Class::Huge.k()],
        mblocks: [geo.mblocks(Class::Small), geo.mblocks(Class::Huge)],
        expect: [
            geo.slots(Class::Small) / shards_for(cores, geo, Class::Small) as u64,
            geo.slots(Class::Huge) / shards_for(cores, geo, Class::Huge) as u64,
        ],
        shards_for: [
            shards_for(cores, geo, Class::Small),
            shards_for(cores, geo, Class::Huge),
        ],
        occ: occ_per_core(cfg.policy.occ_bytes, cores),
        recheck: true,
    }
}

fn class_of(huge: bool) -> Class {
    if huge { Class::Huge } else { Class::Small }
}

impl Allocator {
    /// The core that owns an address's index shard, and so allocates for it. Reuses the
    /// consensus group mapping so the lookup rides the hop consensus already makes.
    fn owner(&self, addr: GlobalAddr, class: Class) -> CoreId {
        let i = server::config().group(addr.0).index() as usize
            % shards_for(self.cores, &self.geo, class);
        CoreId::of(i)
    }

    /// The extent's page kind, class and tombstone epoch, from one lookup.
    fn extent(&self, addr: GlobalAddr) -> Option<(Kind, Class, u64)> {
        let cfg = server::config();
        let e = cfg.extent_at(addr.0)?;
        Some((
            e.kind,
            if e.huge { Class::Huge } else { Class::Small },
            e.tombstone_epoch,
        ))
    }

    /// The extent's page kind, and whether the address falls in the huge class.
    pub fn kind_of(&self, addr: GlobalAddr) -> Result<(Kind, bool), Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        Ok((kind, class == Class::Huge))
    }

    /// The core owning an address. The cache shards by this too, so its lookups are free.
    pub fn owner_core(&self, addr: GlobalAddr) -> Result<CoreId, Status> {
        let (_, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        Ok(self.owner(addr, class))
    }

    /// Cores an address of `class` can hop to, and so the only cores the cache may hold
    /// slots of that class on: a slot anywhere else is unreachable via `owner_core`.
    pub fn shards_for(&self, class: Class) -> usize {
        shards_for(self.cores, &self.geo, class)
    }

    /// The registered device, shared with the cache, which holds the tail of the namespace.
    pub fn disk(&self) -> Disk {
        self.disk.clone()
    }

    /// Where the layout ends and the cache tail begins.
    pub fn geometry(&self) -> Geometry {
        self.geo
    }

    /// Free-space state of this core's shards; group hashing makes it representative.
    pub fn pressure(&self) -> Pressure {
        shard(|sh| sh.pressure())
    }

    /// Whether the store's rate budget is committed far enough ahead that optional work
    /// should stand down. A separate axis from `pressure`, which is about free space.
    pub fn store_pressed(&self) -> bool {
        self.disk.pressed()
    }

    /// Total time transfers have been held back by the rate budget, in microseconds.
    pub fn store_waited_us(&self) -> u64 {
        self.disk.waited_us()
    }

    /// Free and total page slots in this core's shards, small then huge. Per core
    /// because that is where the free lists live; the exporter sums them.
    pub fn capacity(&self) -> [(u64, u64); 2] {
        shard(|sh| sh.capacity())
    }

    /// Live and tombstoned pages per extent in this core's shards,
    /// `(extent, live, tombstones)`. Per core like `capacity`; the exporter sums them.
    pub fn census(&self) -> Vec<(u32, u64, u64)> {
        shard(|sh| sh.census())
    }

    pub fn cores(&self) -> usize {
        self.cores
    }

    // ------------------------------------------------------------------------- loans

    /// Give the allocator a way back to the cache. Once at startup, before any worker runs.
    pub fn attach(&self, cache: &'static Cache) {
        let _ = self.cache.set(cache);
    }

    /// Lend the cache one free 4 MiB data slot from this core's stripe. A lent slot still
    /// counts as free, so a loan cannot move the watermarks lending is gated on. What the
    /// cache wrote is unreachable after a restart: the entry says free.
    pub fn lend(&self) -> Option<Loan> {
        let slot = shard(|sh| sh.lend())?;
        Some(Loan {
            slot,
            off: self.geo.slot_off(Class::Huge, slot),
        })
    }

    /// Call loans back until this core's 4 MiB stripe has a real free reserve again.
    /// Synchronous and same-core because `Shard::reserve` cannot await: a loan must be
    /// recoverable inside the reservation that needs it, and the cache holds each loan on
    /// the lending core, which owns the matching cache stripe. Usually a no-op: lending
    /// stops above `1/LEND_CEILING` free, reclaim starts below `1/LEND_FLOOR`.
    ///
    /// Takes the shard already open, and keeps it open across the hand-back: what the
    /// cache gives up is its own share of this same core, a different borrow entirely.
    fn top_up_in(&self, sh: &mut Shard, class: Class) {
        if class != Class::Huge {
            return;
        }
        let Some(cache) = self.cache.get() else {
            return;
        };
        let (free, total) = sh.capacity()[Class::Huge as usize];
        let lent = sh.lent();
        let want = (total / LEND_FLOOR)
            .saturating_sub(free.saturating_sub(lent))
            .min(lent)
            .min(LEND_BATCH);
        for _ in 0..want {
            let Some(loan) = cache.give_back() else {
                break;
            };
            sh.reclaim(loan.slot);
        }
    }

    // ------------------------------------------------------------------ reservations

    /// Guard check plus free-list pop, all on the owning core. Synchronous: no IO is
    /// issued, so a refusal costs nothing. A present `guard` is the collision detector
    /// and the whole type check: LWW, OCC and Immutable differ only in which version the
    /// proposer presented. `None` asks the shard to derive it from the local row.
    fn reserve_in(
        &self,
        sh: &mut Shard,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        guard: Option<u64>,
        ballot: Ballot,
    ) -> Result<Ticket, Status> {
        let epoch = server::config().tombstone_epoch_of(addr.0);
        self.top_up_in(sh, class);
        sh.reserve(addr, kind, class, guard, ballot, epoch)
    }

    /// The owning core's half of a commit: install the entry, retire the previous slot,
    /// mark the mblock dirty. Returns what `flush_until` must reach for durability.
    fn stage_in(
        &self,
        sh: &mut Shard,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        crc: u32,
    ) -> Result<Option<Staged>, Status> {
        let kind = self.extent(addr).ok_or(Status::Unmapped)?.0;
        maps!(&server::config(), m);
        sh.stage(addr, kind, class, t, crc, &m)
    }

    /// Undo a reservation whose data write failed, so the slot is not leaked.
    fn unreserve_in(&self, sh: &mut Shard, class: Class, t: Ticket) {
        maps!(&server::config(), m);
        sh.unreserve(class, t, &m);
    }

    // ------------------------------------------------------------------- group commit

    /// Wait until mblock `li` is durable at or past `need`. The first arrival issues the
    /// write at once with no timer; commits made while it is in flight ride the next one.
    async fn flush_until(&'static self, class: Class, li: u32, need: u64) -> Result<(), Status> {
        loop {
            match turn(class, li, need) {
                Turn::Done => return Ok(()),
                Turn::Wait => {
                    here(|c| c.commit_parks[class as usize] += 1);
                    Park::new().await;
                }
                Turn::Go(mark) => self.flush(li, mark).await?,
            }
        }
    }

    /// Serialise one mblock from DRAM to the copy that is not current: a whole 4 KiB
    /// block, so there is nothing to read first.
    async fn flush(&'static self, li: u32, mut mark: Flushing) -> Result<(), Status> {
        let class = mark.class;
        // Staging is normally pre-held by `tick`; awaiting here is the cold path.
        if here(|c| c.staging[class as usize].is_none()) {
            let b = match PoolBuf::try_alloc(MBLOCK) {
                Some(b) => b,
                None => PoolBuf::alloc(MBLOCK).await,
            };
            here(|c| c.staging[class as usize] = Some(b));
        }
        let (seq, off, buf) = here(|c| {
            let (seq, h, rows) = c.shard.begin_flush(class, li);
            let stage = c.staging[class as usize].as_mut().unwrap();
            layout::put_mblock(stage, h, rows);
            let off = self
                .geo
                .mblock_off(class, h.mblock_id, (h.generation % 2) as u8);
            (seq, off, stage.buf())
        });
        mark.seq = seq;
        let r = self.disk.write(off, buf, Durability::Durable).await;
        mark.settle(r.is_ok());
        r.map_err(|_| Status::Io)
    }

    // -------------------------------------------------------------------- write paths

    /// The proposer's own leg of an accept, stopped short of the register: page durable,
    /// slot held, but the node reads as before, so a lost proposal leaves no trace here.
    pub async fn begin_small(
        &'static self,
        addr: GlobalAddr,
        guard: u64,
        ballot: Ballot,
        page: &PoolBuf,
    ) -> Result<Pending, Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        if class != Class::Small || page.len() != layout::SMALL_PAGE as usize {
            return Err(Status::Unmapped);
        }
        let owner = self.owner(addr, class);
        let t = at(owner, move |c| {
            self.reserve_in(&mut c.shard, addr, kind, class, Some(guard), ballot)
        })
        .await?;
        let (ticket, crc) = self.write_page(addr, class, t, page).await?;
        Ok(Pending {
            addr,
            class,
            ticket,
            crc,
        })
    }

    /// Make the page durable behind a ticket we already hold, handing it back with the
    /// checksum the entry will carry, or spending it on the slot's return if the device
    /// write fails.
    ///
    /// The ticket goes in and comes out because the two outcomes owe different things: a
    /// page on the device still owes an entry, and a page that never landed owes the slot.
    /// Taking it by value is what makes the caller unable to hold one across the failure.
    async fn write_page(
        &'static self,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        page: &PoolBuf,
    ) -> Result<(Ticket, u32), Status> {
        let crc = layout::page_crc(addr.0, t.version, page);
        let off = self.geo.slot_off(class, t.slot);
        if self
            .disk
            .write(off, page.buf(), Durability::Durable)
            .await
            .is_err()
        {
            let owner = self.owner(addr, class);
            at(owner, move |c| self.unreserve_in(&mut c.shard, class, t)).await;
            return Err(Status::Io);
        }
        Ok((t, crc))
    }

    /// [`begin_small`](Self::begin_small) for the 4 MiB class.
    pub async fn begin_huge(
        &'static self,
        addr: GlobalAddr,
        guard: u64,
        ballot: Ballot,
        buf: Buf,
    ) -> Result<Pending, Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        if class != Class::Huge || buf.len() as u64 != layout::HUGE_PAGE {
            return Err(Status::Unmapped);
        }
        let owner = self.owner(addr, class);
        let t = at(owner, move |c| {
            self.reserve_in(&mut c.shard, addr, kind, class, Some(guard), ballot)
        })
        .await?;
        let off = self.geo.slot_off(class, t.slot);
        if self
            .disk
            .write(off, buf, Durability::Durable)
            .await
            .is_err()
        {
            self.abandon(Pending {
                addr,
                class,
                ticket: t,
                crc: 0,
            })
            .await;
            return Err(Status::Io);
        }
        Ok(Pending {
            addr,
            class,
            ticket: t,
            crc: 0,
        })
    }

    /// Install a staged entry, now that a quorum holds the value. A refusal means our row
    /// moved while the peers were answering, so the version is not ours to report.
    pub async fn finish(&'static self, p: Pending) -> Result<u64, Status> {
        let (addr, class, t, crc) = (p.addr, p.class, p.ticket, p.crc);
        let owner = self.owner(addr, class);
        let version = t.version;
        let done = runtime::on_core(owner.index(), move || async move {
            self.commit(addr, class, t, crc).await
        })
        .await?;
        if done {
            Ok(version)
        } else {
            Err(Status::Conflict { current: 0 })
        }
    }

    /// Give the slot back: this proposal never reached a quorum.
    pub async fn abandon(&'static self, p: Pending) {
        let (addr, class, t) = (p.addr, p.class, p.ticket);
        let owner = self.owner(addr, class);
        at(owner, move |c| self.unreserve_in(&mut c.shard, class, t)).await;
    }

    /// The member side of consensus: apply this page iff the guard matches. An error
    /// leaves the register untouched, so a version read off two acceptors has bytes.
    pub async fn accept_small(
        &'static self,
        addr: GlobalAddr,
        guard: u64,
        ballot: Ballot,
        page: &PoolBuf,
    ) -> Result<u64, Status> {
        // Losing the row is a refusal, not a success: an acceptor that acks a value it
        // did not install lends its vote to a quorum that does not exist.
        self.put_small(addr, Some(guard), ballot, page)
            .await?
            .ok_or(Status::Conflict { current: 0 })
    }

    async fn put_small(
        &'static self,
        addr: GlobalAddr,
        guard: Option<u64>,
        ballot: Ballot,
        page: &PoolBuf,
    ) -> Result<Option<u64>, Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        if class != Class::Small || page.len() != layout::SMALL_PAGE as usize {
            return Err(Status::Unmapped);
        }
        let owner = self.owner(addr, class);
        let t = at(owner, move |c| {
            self.reserve_in(&mut c.shard, addr, kind, class, guard, ballot)
        })
        .await?;
        self.finish_small(addr, class, t, page).await
    }

    /// Data first, then the entry that names it; the guarded and unguarded paths share
    /// this. The ordering is about the register, not torn reads (the checksum catches
    /// those): an acceptor that answered no must still read as before, or two failed data
    /// writes leave a version that looks chosen to later recoveries and nobody can serve.
    async fn finish_small(
        &'static self,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        page: &PoolBuf,
    ) -> Result<Option<u64>, Status> {
        let (t, crc) = self.write_page(addr, class, t, page).await?;
        let owner = self.owner(addr, class);
        let version = t.version;
        let done = runtime::on_core(owner.index(), move || async move {
            self.commit(addr, class, t, crc).await
        })
        .await?;
        Ok(done.then_some(version))
    }

    /// The 4 MiB member side. With no checksum, ordering is the only defence against a
    /// torn or lost data write: data must be durable before the entry naming it.
    pub async fn accept_huge(
        &'static self,
        addr: GlobalAddr,
        guard: u64,
        ballot: Ballot,
        buf: Buf,
    ) -> Result<u64, Status> {
        self.put_huge(addr, Some(guard), ballot, buf)
            .await?
            .ok_or(Status::Conflict { current: 0 })
    }

    async fn put_huge(
        &'static self,
        addr: GlobalAddr,
        guard: Option<u64>,
        ballot: Ballot,
        buf: Buf,
    ) -> Result<Option<u64>, Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        if class != Class::Huge || buf.len() as u64 != layout::HUGE_PAGE {
            return Err(Status::Unmapped);
        }
        let owner = self.owner(addr, class);
        let t = at(owner, move |c| {
            self.reserve_in(&mut c.shard, addr, kind, class, guard, ballot)
        })
        .await?;
        self.finish_huge(addr, class, t, buf).await
    }

    async fn finish_huge(
        &'static self,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        buf: Buf,
    ) -> Result<Option<u64>, Status> {
        let owner = self.owner(addr, class);
        let off = self.geo.slot_off(class, t.slot);
        if self
            .disk
            .write(off, buf, Durability::Durable)
            .await
            .is_err()
        {
            at(owner, move |c| self.unreserve_in(&mut c.shard, class, t)).await;
            return Err(Status::Io);
        }
        let version = t.version;
        let done = runtime::on_core(owner.index(), move || async move {
            self.commit(addr, class, t, 0).await
        })
        .await?;
        Ok(done.then_some(version))
    }

    /// One piece of a 4 MiB page a transport split. Pieces go straight into the slot the
    /// page will occupy, on whichever core received them, so nothing copies or crosses a
    /// core. Returns the staged page once the last piece is durable, `None` while a block
    /// is owed: no checksum on this class, so a page must not reach the register short.
    pub async fn put_huge_part(
        &'static self,
        addr: GlobalAddr,
        guard: u64,
        ballot: Ballot,
        proposer: u8,
        off: u32,
        buf: Buf,
    ) -> Result<Option<Pending>, Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        let len = buf.len() as u64;
        let blk = layout::SMALL_PAGE;
        if class != Class::Huge
            || len == 0
            || !len.is_multiple_of(blk)
            || !(off as u64).is_multiple_of(blk)
            || off as u64 + len > layout::HUGE_PAGE
        {
            return Err(Status::Unmapped);
        }
        let key = (guard, ballot.raw(), proposer);
        let owner = self.owner(addr, class);
        let piece = at(owner, move |c| {
            self.open_parts(c, addr, kind, class, key, ballot)
        })
        .await
        .map(|slot| Piece {
            alloc: self,
            owner,
            addr,
            key,
            slot,
        })?;
        let dst = self.geo.slot_off(class, piece.slot()) + off as u64;
        if self
            .disk
            .write(dst, buf, Durability::Durable)
            .await
            .is_err()
        {
            // Dropped rather than left short: the initiator retries the whole command.
            piece.abandon().await;
            return Err(Status::Io);
        }
        let first = (off as u64 / blk) as u32;
        let n = (len / blk) as u32;
        let full = piece.mark(first, n).await;
        Ok(full.map(|ticket| Pending {
            addr,
            class,
            ticket,
            crc: 0,
        }))
    }

    /// Read a staged page back: the slot alone holds the proxied bytes as a whole page.
    pub async fn read_pending(&'static self, p: &Pending, buf: Buf) -> Result<(), Status> {
        let off = self.geo.slot_off(p.class, p.ticket.slot);
        self.disk.read(off, buf).await.map_err(|_| Status::Io)
    }

    /// Find or start an assembly, counting the piece about to be written. Owner core only.
    ///
    /// Answers with the slot rather than the ticket that holds it: the assembly keeps the
    /// ticket until it is whole or dropped, and a piece only needs somewhere to land.
    ///
    /// The guard comes out of the key rather than beside it, so a piece cannot be
    /// reserved under one command's guard and filed under another's.
    fn open_parts(
        &self,
        c: &mut Core,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        key: PartsKey,
        ballot: Ballot,
    ) -> Result<u32, Status> {
        debug_assert_eq!(key.1, ballot.raw(), "a piece is filed under its own ballot");
        let guard = key.0;
        if let Some(i) = find_parts(c, addr, key) {
            let p = &mut c.parts[i];
            p.busy += 1;
            return Ok(p.ticket.slot);
        }
        let t = self.reserve_in(&mut c.shard, addr, kind, class, Some(guard), ballot)?;
        if c.parts.len() >= HUGE_PARTS {
            // Idle assemblies first; if every one of them has a write in flight there is
            // no room, and a refusal the initiator retries beats corrupting a page.
            let Some(i) = c.parts.iter().position(|p| p.busy == 0) else {
                self.unreserve_in(&mut c.shard, class, t);
                return Err(Status::Io);
            };
            let old = c.parts.remove(i);
            self.unreserve_in(&mut c.shard, class, old.ticket);
        }
        let slot = t.slot;
        c.parts.push(Parts {
            addr,
            key,
            ticket: t,
            busy: 1,
            have: [0; (layout::HUGE_PAGE / layout::SMALL_PAGE / 64) as usize],
            blocks: 0,
        });
        Ok(slot)
    }

    /// Record blocks now durable, and take the assembly out once whole. Owner core only.
    fn mark_parts(
        c: &mut Core,
        addr: GlobalAddr,
        key: PartsKey,
        first: u32,
        n: u32,
    ) -> Option<Ticket> {
        let i = find_parts(c, addr, key)?;
        let p = &mut c.parts[i];
        p.busy -= 1;
        for b in first..first + n {
            let (w, bit) = ((b / 64) as usize, 1u64 << (b % 64));
            if p.have[w] & bit == 0 {
                p.have[w] |= bit;
                p.blocks += 1;
            }
        }
        (p.blocks == HUGE_BLOCKS && p.busy == 0).then(|| c.parts.remove(i).ticket)
    }

    /// Drop an assembly; the last piece out gives the slot back, as a sibling may still
    /// be writing. Owed blocks stay unmarked, so the page cannot complete. Owner core only.
    fn drop_parts(&self, c: &mut Core, addr: GlobalAddr, key: PartsKey) {
        let Some(i) = find_parts(c, addr, key) else {
            return;
        };
        c.parts[i].busy -= 1;
        if c.parts[i].busy != 0 {
            return;
        }
        let p = c.parts.remove(i);
        self.unreserve_in(&mut c.shard, Class::Huge, p.ticket);
    }

    // -------------------------------------------------------------- consensus surface

    /// The register as this node holds it, with no data read. A page never seen sits at
    /// version zero, or `3 * epoch` for an Immutable extent, and consensus needs that as
    /// a vote. A page we should hold but cannot serve reports `Missing`, not a vote.
    pub async fn register(&'static self, addr: GlobalAddr) -> Result<Register, Status> {
        let owner = self.owner_core(addr)?;
        at(owner, move |c| self.register_of(&mut c.shard, addr)).await
    }

    /// [`register`](Self::register) without the hop, for a caller already on the owning
    /// core. `paxos` uses it to bump the cache's sketch and read the register in one hop.
    pub fn register_local(&self, addr: GlobalAddr) -> Result<Register, Status> {
        shard(|sh| self.register_of(sh, addr))
    }

    /// [`register`](Self::register) against a shard already open.
    fn register_of(&self, sh: &mut Shard, addr: GlobalAddr) -> Result<Register, Status> {
        let (kind, class, epoch) = self.extent(addr).ok_or(Status::Unmapped)?;
        debug_assert_eq!(runtime::core_id(), self.owner(addr, class));
        sh.register_of(addr, kind, class, epoch)
    }

    /// The guard a proposer on this node should present. The OCC read ring is consulted
    /// here, which is why acceptors need no read-tracking state of their own.
    pub async fn guard(&'static self, addr: GlobalAddr) -> Result<u64, Status> {
        let (kind, class, epoch) = self.extent(addr).ok_or(Status::Unmapped)?;
        at(self.owner(addr, class), move |c| {
            c.shard.guard_of(addr, kind, class, epoch)
        })
        .await
    }

    /// Record a read this node's own ublk device served: the only thing feeding the OCC
    /// ring. Serving a peer is not a read of ours.
    pub async fn observed(&'static self, addr: GlobalAddr, version: u64) {
        let Some((kind, class, _)) = self.extent(addr) else {
            return;
        };
        at(self.owner(addr, class), move |c| {
            c.shard.observed(addr, kind, version);
        })
        .await
    }

    /// The unguarded apply-if-newer write: repair and `learn`, one operation with two
    /// provenances. Legal only underneath a prepare, or into a migration destination.
    /// `Ok(false)` means what we hold is at least as new, which makes migration streams
    /// commutative and a repeated repair free. `replace_equal` also rewrites a row held
    /// at exactly `r`: how a repair replaces bytes that failed checksum at that register.
    pub async fn learn_small(
        &'static self,
        addr: GlobalAddr,
        r: Register,
        page: &PoolBuf,
        replace_equal: bool,
    ) -> Result<bool, Status> {
        let (_, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        if class != Class::Small || page.len() != layout::SMALL_PAGE as usize {
            return Err(Status::Unmapped);
        }
        let Some(t) = self
            .reserve_unguarded(addr, class, r, replace_equal)
            .await?
        else {
            return Ok(false);
        };
        if replace_equal {
            let (t, crc) = self.write_page(addr, class, t, page).await?;
            let owner = self.owner(addr, class);
            return runtime::on_core(owner.index(), move || {
                Box::pin(self.commit_replace_equal(addr, class, t, crc))
            })
            .await;
        }
        Ok(self.finish_small(addr, class, t, page).await?.is_some())
    }

    /// Learn a register with no bytes: an Immutable tombstone. No member has a page to
    /// serve, so the register is the value; a replica missing it diverges at the fill point.
    pub async fn learn_tombstone(
        &'static self,
        addr: GlobalAddr,
        r: Register,
    ) -> Result<bool, Status> {
        let (_, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        let Some(t) = self.reserve_unguarded(addr, class, r, false).await? else {
            return Ok(false);
        };
        let owner = self.owner(addr, class);
        runtime::on_core(owner.index(), move || async move {
            self.commit(addr, class, t, 0).await
        })
        .await
    }

    pub async fn learn_huge(
        &'static self,
        addr: GlobalAddr,
        r: Register,
        buf: Buf,
    ) -> Result<bool, Status> {
        let (_, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        if class != Class::Huge || buf.len() as u64 != layout::HUGE_PAGE {
            return Err(Status::Unmapped);
        }
        let Some(t) = self.reserve_unguarded(addr, class, r, false).await? else {
            return Ok(false);
        };
        Ok(self.finish_huge(addr, class, t, buf).await?.is_some())
    }

    async fn reserve_unguarded(
        &'static self,
        addr: GlobalAddr,
        class: Class,
        r: Register,
        replace_equal: bool,
    ) -> Result<Option<Ticket>, Status> {
        let (kind, _, epoch) = self.extent(addr).ok_or(Status::Unmapped)?;
        at(self.owner(addr, class), move |c| {
            self.top_up_in(&mut c.shard, class);
            c.shard
                .reserve_unguarded(addr, kind, class, r, epoch, replace_equal)
        })
        .await
    }

    async fn commit(
        &'static self,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        crc: u32,
    ) -> Result<bool, Status> {
        // `None` means the row moved while our data write was in flight; the slot went
        // back and there is nothing to make durable.
        let Some(st) = shard(|sh| self.stage_in(sh, addr, class, t, crc))? else {
            return Ok(false);
        };
        self.flush_until(class, st.li, st.seq).await?;
        // Only now is it safe to give the previous slot back.
        if let Some(old) = st.stale {
            maps!(&server::config(), m);
            shard(|sh| sh.release(class, old, &m));
        }
        Ok(true)
    }

    /// Equal-register replacement has no ordering key that startup can use to choose
    /// between duplicate rows, so repair cannot acknowledge until the old row is gone.
    async fn commit_replace_equal(
        &'static self,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        crc: u32,
    ) -> Result<bool, Status> {
        let Some(st) = shard(|sh| self.stage_in(sh, addr, class, t, crc))? else {
            return Ok(false);
        };
        self.flush_until(class, st.li, st.seq).await?;
        let Some(slot) = st.stale else {
            return Ok(true);
        };
        let holder = CoreId::of((slot / class.k() % self.cores as u32) as usize);
        let retire = move || async move {
            maps!(&server::config(), m);
            let flush = shard(|sh| sh.release(class, slot, &m));
            if let Some((li, seq)) = flush {
                self.flush_until(class, li, seq).await?;
            }
            Ok(())
        };
        if holder == runtime::core_id() {
            retire().await
        } else {
            runtime::on_core(holder.index(), retire).await
        }?;
        Ok(true)
    }

    // --------------------------------------------------------------------- read paths

    /// 4 KiB read, verified against the entry's seeded CRC; a mismatch is never served.
    pub async fn read_small(
        &'static self,
        addr: GlobalAddr,
        page: &mut PoolBuf,
    ) -> Result<Register, Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        if class != Class::Small || page.len() != layout::SMALL_PAGE as usize {
            return Err(Status::Unmapped);
        }
        let owner = self.owner(addr, class);
        let l = at(owner, move |c| c.shard.lookup(addr, kind, class)).await?;
        let off = self.geo.slot_off(class, l.slot);
        self.disk
            .read(off, page.buf())
            .await
            .map_err(|_| Status::Io)?;
        if layout::page_crc(addr.0, l.version, page) != l.crc {
            return Err(Status::Missing);
        }
        Ok(Self::reg_of(&l))
    }

    /// 4 MiB read into the caller's buffer, which may be guest pages. No checksum here.
    pub async fn read_huge(
        &'static self,
        addr: GlobalAddr,
        off: usize,
        buf: Buf,
    ) -> Result<Register, Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        if class != Class::Huge || off as u64 + buf.len() as u64 > layout::HUGE_PAGE {
            return Err(Status::Unmapped);
        }
        let owner = self.owner(addr, class);
        let l = at(owner, move |c| c.shard.lookup(addr, kind, class)).await?;
        let read = self.geo.slot_off(class, l.slot) + off as u64;
        self.disk.read(read, buf).await.map_err(|_| Status::Io)?;
        // Nothing held the slot still while those bytes were read. The entry could have
        // been trimmed, its slot given back, and the slot reserved and written for some
        // other address, and a 4 MiB page has no checksum to disagree with the result.
        //
        // Asking again is enough to rule that out. A version only ever moves forward, so
        // an entry that answers with the same slot at the same register is an entry that
        // never left: had the slot been freed, getting back to this address would have
        // taken a write, and a write would have taken a higher version. The 4 KiB path
        // gets the same guarantee from a checksum seeded with the address and version.
        if at(owner, move |c| c.shard.lookup(addr, kind, class)).await? != l {
            return Err(Status::Missing);
        }
        Ok(Self::reg_of(&l))
    }

    /// Serve a hole by reading the format-time zero region: one device read, no memset.
    pub async fn read_zeroes(&'static self, buf: Buf) -> Result<(), Status> {
        self.disk
            .read(self.geo.zero_base, buf)
            .await
            .map_err(|_| Status::Io)
    }

    /// The register the bytes just read belong to. Read separately, an accept could land
    /// in between and a value travel under a version it was never written at.
    fn reg_of(l: &Lookup) -> Register {
        Register {
            version: l.version,
            ballot: Ballot::from_raw(l.ballot as u32),
        }
    }

    // ----------------------------------------------------------------------- discard

    /// The member side of a trim proposal, which only the immutable class has: the page
    /// becomes a tombstone, so a reader can tell a hole from a trim, reclaimed once the
    /// control plane advances the epoch past it. Guard is `3*epoch + 1`; a repeat is `Ok`.
    /// A mutable page discards by accepting zeroes, so a trim of one is refused here.
    pub async fn accept_trim(
        &'static self,
        addr: GlobalAddr,
        guard: u64,
        ballot: Ballot,
    ) -> Result<(), Status> {
        self.trim_at(addr, Some(guard), ballot).await
    }

    async fn trim_at(
        &'static self,
        addr: GlobalAddr,
        guard: Option<u64>,
        ballot: Ballot,
    ) -> Result<(), Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        let owner = self.owner(addr, class);
        // The mblock index is the owner's, so the flush has to happen there too.
        runtime::on_core(owner.index(), move || async move {
            match self.trim(addr, kind, class, guard, ballot)? {
                Some((li, need)) => self.flush_until(class, li, need).await,
                None => Ok(()),
            }
        })
        .await
    }

    fn trim(
        &self,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        guard: Option<u64>,
        ballot: Ballot,
    ) -> Result<Option<(u32, u64)>, Status> {
        let epoch = server::config().tombstone_epoch_of(addr.0);
        maps!(&server::config(), m);
        shard(|sh| sh.trim(addr, kind, class, guard, ballot, epoch, &m))
    }

    // -------------------------------------------------------- consensus side state

    /// `promised_term` and the seal table as the superblock held them at startup.
    pub fn boot_consensus(&self) -> &layout::Consensus {
        &self.boot
    }

    /// Rewrite the consensus side state through every superblock copy, preserving the
    /// geometry words. Read-modify-write, not batched: terms bump on repair, restart and
    /// reconfiguration only, never on the hot path. An acked seal that is then lost is a
    /// shard writable in two zones, hence fourfold redundancy, not the mblocks' A/B.
    pub async fn save_consensus(&'static self, c: &layout::Consensus) -> Result<(), Status> {
        let mut b = PoolBuf::alloc(MBLOCK).await;
        let mut found = false;
        for i in 0..layout::sb_copies() {
            if self.disk.read(layout::sb_off(i), b.buf()).await.is_ok() && layout::sb_valid(&b) {
                found = true;
                break;
            }
        }
        if !found {
            return Err(Status::Io);
        }
        c.patch(&mut b);
        for i in 0..layout::sb_copies() {
            self.disk
                .write(layout::sb_off(i), b.buf(), Durability::Durable)
                .await
                .map_err(|_| Status::Io)?;
        }
        Ok(())
    }

    // ------------------------------------------------------------------- anti-entropy

    /// The core that holds a group's registers, and so its digest and cursors. The index
    /// alone, not the whole id: universes reuse the same core layout, keeping this in
    /// step with `owner` and a page's shard on the same core as its consensus group.
    fn owner_of(&self, group: GroupId, class: Class) -> CoreId {
        CoreId::of(group.index() as usize % shards_for(self.cores, &self.geo, class))
    }

    /// This node's digest vector for one group and class. Boxed because it crosses a
    /// core boundary and the runtime's reply payload is small.
    pub async fn digests(&'static self, group: GroupId, huge: bool) -> Box<[u64; heal::BUCKETS]> {
        let class = class_of(huge);
        at(self.owner_of(group, class), move |c| {
            c.shard.digest_vector(class, group)
        })
        .await
    }

    /// Open an enumeration of a group's registers, narrowed by `filter`.
    pub async fn snap_open(
        &'static self,
        group: GroupId,
        huge: bool,
        filter: heal::Filter,
    ) -> Result<Snapshot, Status> {
        let class = class_of(huge);
        let core = self.owner_of(group, class);
        let id = at(core, move |c| {
            let now = runtime::now();
            c.shard
                .snap_start(class, core.index(), huge, group, filter, now)
                .ok_or(Status::NoSpace)
        })
        .await?;
        Ok(Snapshot { alloc: self, id })
    }

    /// Next chunk. Bounded by entries scanned as well as tuples produced, so a sparse
    /// filter costs more frames but never a long stall on the owning core. `universe` is
    /// the request's namespace, `None` for a local caller; a cursor answers only its own.
    pub async fn snap_next(
        &'static self,
        id: u32,
        seq: Option<u8>,
        universe: Option<u32>,
    ) -> Result<(Vec<Tuple>, bool), Status> {
        let (core, huge, _, _) = heal::snap_parts(id);
        if core >= self.cores {
            return Err(Status::Unmapped);
        }
        let class = class_of(huge);
        at(CoreId::of(core), move |c| {
            let now = runtime::now();
            maps!(&server::config(), m);
            c.shard.snap_next(class, id, seq, universe, &m, now)
        })
        .await
    }

    /// Close a cursor. Idempotent, and the last one out resumes reclamation.
    pub async fn snap_release(&'static self, id: u32) {
        let (core, huge, _, _) = heal::snap_parts(id);
        if core >= self.cores {
            return;
        }
        let class = class_of(huge);
        at(CoreId::of(core), move |c| c.shard.snap_stop(class, id)).await
    }

    // ------------------------------------------------------------------------- shed

    /// Groups this core still holds registers for, in one class. Per core like `census`:
    /// a group's registers, and its digests, all live on the one core its id maps to.
    pub fn held_groups(&self, huge: bool) -> Vec<GroupId> {
        shard(|sh| sh.held_groups(class_of(huge)))
    }

    /// Forget a group this core has been drained of, so it drops out of `held_groups`.
    pub async fn forget_group(&'static self, group: GroupId, huge: bool) {
        let class = class_of(huge);
        at(self.owner_of(group, class), move |c| {
            c.shard.forget_group(class, group);
        })
        .await
    }

    /// Drop a register this node no longer owns. Nothing checks that: the caller has
    /// confirmed the new holders. Refuses if the register moved, making that stale.
    pub async fn discard(&'static self, addr: GlobalAddr, version: u64) -> Result<(), Status> {
        let (_, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        let owner = self.owner(addr, class);
        // The mblock index is the owner's, so the flush has to happen there too.
        runtime::on_core(owner.index(), move || async move {
            maps!(&server::config(), m);
            // Bound before the match: a temporary in the scrutinee would hold the shard
            // borrow across the flush's await.
            let hit = shard(|sh| sh.discard(addr, class, version, &m));
            match hit {
                Some((li, need)) => self.flush_until(class, li, need).await,
                None => Ok(()),
            }
        })
        .await
    }

    // -------------------------------------------------------------------- maintenance

    /// Cooperative maintenance, called from the runtime's tick on every worker. Takes the
    /// Take each class's mblock staging buffer once, then sweep a bounded slice of this
    /// core's tombstones:
    /// the only garbage collection in the system, metadata only, off any critical path.
    pub fn tick(&self, now: Instant) {
        let cfg = server::config();
        maps!(&cfg, m);
        // Nothing to reclaim until the configuration can retire something.
        let reclaimable = cfg.reclaimable();
        here(|c| {
            for class in [Class::Small, Class::Huge] {
                if c.staging[class as usize].is_none() {
                    c.staging[class as usize] = PoolBuf::try_alloc(MBLOCK);
                }
                c.shard.snap_expire(class, now);
                if reclaimable {
                    c.shard.sweep(class, &m);
                }
            }
            c.shard
                .set_occ(occ_per_core(cfg.policy.occ_bytes, self.cores));
            c.shard.set_recoverable(cfg.peer_count() > 0);
        });
    }

    /// This core's group-commit counters.
    pub fn local_stats(&self) -> Stats {
        here(|c| {
            let mut out = Stats::default();
            let swept = c.shard.sweep_stats();
            for (i, (commits, flushes, flush_batch)) in
                c.shard.flush_stats().into_iter().enumerate()
            {
                out.per[i] = ClassStats {
                    commits,
                    flushes,
                    flush_batch,
                    parks: c.commit_parks[i],
                    busy_us: c.flush_busy_us[i],
                    swept_epoch: swept[i].0,
                    swept_uncovered: swept[i].1,
                };
            }
            out
        })
    }
}

// ------------------------------------------------------------------- enumeration

/// An open walk over a group's registers.
///
/// A cursor defers reclamation on the slab it is walking, and a slab holds only so many
/// at once, so one abandoned part-way costs both until its deadline passes. Every local
/// reader takes a chunk in a loop and gives up on the first error, which used to leave
/// the cursor open for a whole timeout each time a peer went quiet mid-walk.
///
/// A cursor handed to the wire is a different thing and leaves through
/// [`into_wire`](Self::into_wire): the peer that asked for it owns it from then on, and
/// the deadline is what covers a peer that never comes back to close it.
#[must_use = "an open cursor defers reclamation on its slab until it is closed"]
pub struct Snapshot {
    alloc: &'static Allocator,
    id: u32,
}

impl Snapshot {
    /// The next chunk, and whether it was the last.
    pub async fn next(&self) -> Result<(Vec<Tuple>, bool), Status> {
        self.alloc.snap_next(self.id, None, None).await
    }

    /// Close the walk, resuming reclamation once this was the last cursor out.
    pub async fn close(self) {
        let me = std::mem::ManuallyDrop::new(self);
        me.alloc.snap_release(me.id).await;
    }

    /// Give the cursor to whoever asked for it over the wire, along with the closing.
    pub fn into_wire(self) -> u32 {
        let me = std::mem::ManuallyDrop::new(self);
        me.id
    }
}

impl Drop for Snapshot {
    fn drop(&mut self) {
        let (alloc, id) = (self.alloc, self.id);
        // Detached, because a destructor cannot await. If the slab has no room the cursor
        // waits out its deadline, which is what abandoning one did every time before this.
        let _ = runtime::spawn(async move { alloc.snap_release(id).await });
    }
}

// ------------------------------------------------------------------ huge assembly

/// One piece of a 4 MiB page, on its way into the slot an assembly holds.
///
/// Opening a piece counts a write in flight, which is what stops the assembly being
/// evicted and its slot reused while a sibling is still landing bytes in it. A future
/// dropped between opening a piece and reporting it left that count raised for good: the
/// page could never complete, because completion asks that no write is still in flight,
/// and the assembly could never be evicted either, so its slot stayed reserved for the
/// life of the process.
///
/// Reporting is the only way to spend one, and there is no way to make a second for the
/// same write, so the count now comes back down however the piece ends.
#[must_use = "an unreported piece leaves a write counted in flight for good"]
struct Piece {
    alloc: &'static Allocator,
    owner: CoreId,
    addr: GlobalAddr,
    key: PartsKey,
    slot: u32,
}

impl Piece {
    /// The slot these bytes belong in. Every piece of one command shares it.
    fn slot(&self) -> u32 {
        self.slot
    }

    /// Report blocks durable, answering with the ticket once the page is whole.
    async fn mark(self, first: u32, n: u32) -> Option<Ticket> {
        let me = std::mem::ManuallyDrop::new(self);
        let (owner, addr, key) = (me.owner, me.addr, me.key);
        at(owner, move |c| {
            Allocator::mark_parts(c, addr, key, first, n)
        })
        .await
    }

    /// Give the piece up. The assembly goes with it once no sibling is still writing.
    async fn abandon(self) {
        let me = std::mem::ManuallyDrop::new(self);
        let (alloc, owner, addr, key) = (me.alloc, me.owner, me.addr, me.key);
        at(owner, move |c| alloc.drop_parts(c, addr, key)).await;
    }
}

impl Drop for Piece {
    fn drop(&mut self) {
        let (alloc, owner, addr, key) = (self.alloc, self.owner, self.addr, self.key);
        // Detached, because a destructor cannot await and the assembly may be another
        // core's. If the slab has no room the count stays raised, which is what dropping
        // a piece did every time before this.
        let _ = runtime::spawn(async move {
            at(owner, move |c| alloc.drop_parts(c, addr, key)).await;
        });
    }
}

// -------------------------------------------------------------------- group commit

/// [`Act`] with the claim it hands out.
///
/// `Go` is not a fact about the slab so much as a claim on it, so it comes back as the
/// thing that has to be given up rather than as a word saying it was taken.
enum Turn {
    Done,
    Wait,
    Go(Flushing),
}

/// This core's turn at flushing mblock `li`, waiting or nothing if it is not owed one.
fn turn(class: Class, li: u32, need: u64) -> Turn {
    match shard(|sh| sh.flush_act(class, li, need)) {
        Act::Done => Turn::Done,
        Act::Wait => Turn::Wait,
        // The slab has just marked itself busy for us, and this is the only thing that
        // will unmark it. Built here and nowhere else for that reason.
        Act::Go => {
            here(|c| c.flush_started[class as usize] = Some(runtime::now()));
            Turn::Go(Flushing { class, li, seq: 0 })
        }
    }
}

/// A slab marked busy for one flush, and the obligation to unmark it.
///
/// Between taking the mark and retiring it are two awaits: the staging buffer on the cold
/// path, and the write itself. A future dropped at either of them used to leave the mark
/// set for the rest of the process: every committer on this core would park behind a
/// flush that was never going to finish, and no later flush could take the slab either.
/// Retiring it from a destructor makes the mark last exactly as long as the attempt does,
/// whether the attempt finishes or is abandoned.
#[must_use = "an unretired flush leaves the slab busy and every committer behind it parked"]
struct Flushing {
    class: Class,
    li: u32,
    /// The sequence this write makes durable, known only once `begin_flush` has run.
    seq: u64,
}

impl Flushing {
    /// Retire a flush that ran, recording whether the write landed.
    fn settle(self, ok: bool) {
        let me = std::mem::ManuallyDrop::new(self);
        Flushing::retire(me.class, me.li, me.seq, ok);
    }

    /// Give the slab back and wake everyone waiting on it. Synchronous, and on this core,
    /// because the mark is this core's: a destructor cannot await, and here it need not.
    fn retire(class: Class, li: u32, seq: u64, ok: bool) {
        here(|c| {
            let ci = class as usize;
            if let Some(started) = c.flush_started[ci].take() {
                c.flush_busy_us[class as usize] += runtime::now()
                    .saturating_duration_since(started)
                    .as_micros() as u64;
            }
            c.shard.end_flush(class, li, seq, ok);
            for w in c.waiters.drain(..) {
                w.wake();
            }
        });
    }
}

impl Drop for Flushing {
    fn drop(&mut self) {
        // Abandoned part-way, so nothing is claimed durable: the write may have landed or
        // may never have been issued, and a sequence claimed wrongly is an acknowledged
        // page lost. The waiters are woken all the same, because what they wait for is a
        // turn and not this particular write.
        Flushing::retire(self.class, self.li, self.seq, false);
    }
}

// -------------------------------------------------------------------------- futures

/// Yield once and resume when a flush completes on this core.
///
/// It names no core: the waker goes into the row of whichever worker polls it, which is
/// the worker running the commit that is waiting, which is the worker that will flush.
struct Park {
    armed: bool,
}

impl Park {
    fn new() -> Park {
        Park { armed: false }
    }
}

impl Future for Park {
    type Output = ();

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if self.armed {
            return Poll::Ready(());
        }
        self.armed = true;
        here(|c| c.waiters.push(cx.waker().clone()));
        Poll::Pending
    }
}

// --------------------------------------------------------------------------- startup

/// Open a formatted device: validate the superblock, rebuild every shard by scanning the
/// metadata region, and hand back an allocator that never needs a journal replay.
/// Runs on the control thread before any worker sees traffic, so the scan can use plain
/// threads and blocking reads metered against one shared budget. The returned reference
/// is leaked: the allocator lives for the process and hop closures must be `'static`.
pub fn open(
    path: &std::path::Path,
    disk: Disk,
    cfg: &Config,
    cores: usize,
) -> std::io::Result<(&'static Allocator, Vec<Row>)> {
    let geo = layout::read_geometry(path)?;
    let boot = layout::read_consensus(path)?;
    let limit = std::sync::Arc::new(runtime::Limiter::new(
        cfg.node.store_max_iops,
        cfg.node.store_max_bytes_per_sec,
    ));
    let scans = scan(path, &geo, cores, &limit)?;

    // Nothing to heal a lost mblock from without peers, so a miss there stays a hole.
    let recoverable = cfg.peer_count() > 0;
    let (shards, quarantined) = {
        maps!(cfg, m);
        shard::rebuild(&shape_of(&geo, cfg, cores), cores, scans, &m)
    };
    let rows = shards
        .into_iter()
        .map(|mut shard| {
            shard.set_recoverable(recoverable);
            Row::new(shard)
        })
        .collect::<Vec<_>>();

    let alloc = Box::leak(Box::new(Allocator {
        disk,
        geo,
        cores,
        boot,
        quarantined,
        cache: OnceLock::new(),
    }));
    Ok((alloc, rows))
}

/// Read the whole metadata region and resolve each mblock's A/B copies. Split into
/// contiguous ranges, not by owning core, so threads read sequentially; striping after.
fn scan(
    path: &std::path::Path,
    geo: &Geometry,
    threads: usize,
    limit: &std::sync::Arc<runtime::Limiter>,
) -> std::io::Result<Vec<shard::Scanned>> {
    let mut out = Vec::new();
    for class in [Class::Small, Class::Huge] {
        let n = geo.mblocks(class);
        if n == 0 {
            continue;
        }
        let per = n.div_ceil(threads as u64);
        // The simulator's device is thread-local, so it has to scan in one pass.
        #[cfg(feature = "sim")]
        let parts: Vec<std::io::Result<Vec<shard::Scanned>>> = {
            let _ = per;
            vec![scan_range(path, geo, class, 0, n, limit)]
        };
        #[cfg(not(feature = "sim"))]
        let parts: Vec<std::io::Result<Vec<shard::Scanned>>> = std::thread::scope(|s| {
            let hs: Vec<_> = (0..threads)
                .map(|t| {
                    let lo = (t as u64 * per).min(n);
                    let hi = ((t as u64 + 1) * per).min(n);
                    s.spawn(move || scan_range(path, geo, class, lo, hi, limit))
                })
                .collect();
            hs.into_iter().map(|h| h.join().unwrap()).collect()
        });
        for p in parts {
            out.extend(p?);
        }
    }
    Ok(out)
}

fn scan_range(
    path: &std::path::Path,
    geo: &Geometry,
    class: Class,
    lo: u64,
    hi: u64,
    limit: &std::sync::Arc<runtime::Limiter>,
) -> std::io::Result<Vec<shard::Scanned>> {
    let mut out = Vec::new();
    if lo >= hi {
        return Ok(out);
    }
    let f = layout::open_direct(path, false)?.meter(limit.clone());
    const BATCH: u64 = 1024;
    let mut a = layout::Aligned::new(BATCH as usize * MBLOCK);
    let mut b = layout::Aligned::new(BATCH as usize * MBLOCK);
    let mut at = lo;
    while at < hi {
        // One batch never crosses an extent boundary: the blocks past it live elsewhere
        // on the device, and both copies are only contiguous within a run.
        let n = BATCH.min(hi - at).min(geo.ext_end(class, at) - at);
        let len = n as usize * MBLOCK;
        layout::read_at(
            &f,
            &mut a.as_mut()[..len],
            geo.mblock_off(class, at as u32, 0),
        )?;
        layout::read_at(
            &f,
            &mut b.as_mut()[..len],
            geo.mblock_off(class, at as u32, 1),
        )?;
        for i in 0..n as usize {
            let id = (at + i as u64) as u32;
            let ba = &a.as_ref()[i * MBLOCK..(i + 1) * MBLOCK];
            let bb = &b.as_ref()[i * MBLOCK..(i + 1) * MBLOCK];
            // A header naming another block or class is not this block's copy.
            let ha = layout::get_header(ba).filter(|h| h.mblock_id == id && h.class == class);
            let hb = layout::get_header(bb).filter(|h| h.mblock_id == id && h.class == class);
            match shard::pick_ab(ha, hb).map(|(h, b)| (h, if b { bb } else { ba })) {
                Some((h, raw)) => {
                    let k = class.k();
                    let mut entries = Vec::with_capacity(k as usize);
                    for j in 0..k {
                        entries.push(layout::get_entry(raw, class, j));
                    }
                    out.push(shard::Scanned {
                        id,
                        class,
                        generation: h.generation,
                        quarantined: false,
                        entries,
                    });
                }
                None => out.push(shard::Scanned {
                    id,
                    class,
                    generation: 0,
                    quarantined: true,
                    entries: Vec::new(),
                }),
            }
        }
        at += n;
    }
    Ok(out)
}

// --- Invariants, for the sim ---

/// What must be true of the allocator between actions, whatever the fabric is doing to
/// it. Sampled by the simulator after every step a fuzzer takes, so a violation is
/// caught at the action that caused it rather than at the read that eventually noticed.
#[cfg(feature = "sim")]
impl Row {
    /// What must hold of one core's share whatever has been done to it. The simulator
    /// samples this between steps; nothing else calls it.
    pub(crate) fn invariants(&self) -> Result<(), String> {
        // A borrow held here means a worker is mid-mutation, which only happens if
        // this was called from inside the runtime. Say so rather than panicking.
        let Ok(c) = self.0.try_borrow() else {
            return Err("the shard is borrowed".into());
        };
        c.shard.invariants()?;
        if c.parts.len() > HUGE_PARTS {
            return Err(format!(
                "{} assemblies, past the {HUGE_PARTS} the table holds",
                c.parts.len()
            ));
        }
        for (j, p) in c.parts.iter().enumerate() {
            if p.blocks > HUGE_BLOCKS {
                return Err(format!(
                    "assembly {j} claims {} blocks of a {HUGE_BLOCKS} block page",
                    p.blocks
                ));
            }
            // Two commands for one address may be in flight at once, but never two
            // for the same command: their pieces would land in each other's slot.
            if c.parts[..j]
                .iter()
                .any(|q| q.addr == p.addr && q.key == p.key)
            {
                return Err(format!(
                    "two assemblies for {:#x} under one command",
                    p.addr.0
                ));
            }
        }
        Ok(())
    }

    /// Pages part-way through arriving. A reservation outlives the command that opened it
    /// only until the assembly is evicted, so a cluster left to settle holds none.
    pub(crate) fn assemblies(&self) -> usize {
        self.0.try_borrow().map(|c| c.parts.len()).unwrap_or(0)
    }
}

// --- Tests ---

/// The allocator on its own, without ublk. Pins durability across a restart with no
/// journal, the three type semantics, and the asymmetry where a corrupted 4 KiB page is
/// refused and a corrupted 4 MiB page is not. In-crate so the allocator's surface and
/// `layout`'s raw disk helpers stay crate-private. Needs root only because the runtime
/// reads the ublk feature set; no device is created.
#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::path::Path;
    use std::pin::Pin;
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::task::{Context, Waker};
    use std::time::{Duration, Instant};

    use super::{Allocator, GlobalAddr, Status};
    use crate::config::Config;
    use crate::layout::{self, Class, State};
    use crate::paxos::Ballot;
    use crate::runtime::{self, Cfg, Errno, Handler, PoolBuf, Request};
    use crate::server::{self, Dataplane};

    const IMG: &str = "racer-alloc.img";
    /// Just what the base config plans, so appended runs need a larger store, not slack.
    const DEV_BYTES: u64 = 528 << 20;
    /// What the store has to be told to be before the appended runs fit in it.
    const GROWN_BYTES: u64 = 1068 << 20;
    const SMALL: usize = 4096;
    const HUGE: usize = 4 << 20;

    // --- harness ---

    struct Driver;
    static DRIVER: Driver = Driver;

    static PHASE: AtomicUsize = AtomicUsize::new(0);
    static RESULT: Mutex<Option<Result<(), String>>> = Mutex::new(None);
    static STARTED: AtomicUsize = AtomicUsize::new(0);
    /// LWW addresses whose entries were in the mblock the test wrecked.
    static LOST: Mutex<Vec<u64>> = Mutex::new(Vec::new());

    impl Handler for Driver {
        type Config = Dataplane;
        type CoreState = server::CoreState;

        async fn handle(&'static self, _cfg: Cfg<Dataplane>, _req: Request) -> Result<(), Errno> {
            Err(Errno::EOPNOTSUPP)
        }

        fn tick(&'static self, cfg: Cfg<Dataplane>, now: Instant) {
            cfg.alloc().tick(now);
            // Launch the script once, from core 0 onto core 1, to exercise the cross-core
            // path. `runtime::spawn` is core-local, so only a hop carries a task
            // elsewhere: polling once sends the message, dropping it abandons the reply.
            if runtime::core() != 0 || STARTED.swap(1, Ordering::SeqCst) == 1 {
                return;
            }
            let a = cfg.alloc();
            let phase = PHASE.load(Ordering::SeqCst);
            let mut hop = Box::pin(runtime::on_core(1, move || boxed(a, phase)));
            let w = Waker::noop();
            let _ = hop.as_mut().poll(&mut Context::from_waker(w));
        }
    }

    /// Boxed so the hop's payload and task-slot size limits see a fat pointer.
    fn boxed(a: &'static Allocator, phase: usize) -> Pin<Box<dyn Future<Output = ()>>> {
        Box::pin(async move {
            let r = script(a, phase).await.map_err(|f| f.0);
            *RESULT.lock().unwrap() = Some(r);
        })
    }

    fn run(dev: &Path, phase: usize) {
        run_with(dev, phase, &config_text(dev));
    }

    fn run_with(_dev: &Path, phase: usize, text: &str) {
        PHASE.store(phase, Ordering::SeqCst);
        STARTED.store(0, Ordering::SeqCst);
        *RESULT.lock().unwrap() = None;

        let cfg = Config::parse(text).unwrap();
        let rt = runtime::start(&DRIVER).expect("start");
        rt.reload(move |c| server::bare_plane(c, cfg))
            .expect("reload");

        let deadline = Instant::now() + Duration::from_secs(120);
        loop {
            let done = RESULT.lock().unwrap().take();
            if let Some(r) = done {
                rt.shutdown().unwrap();
                if let Err(e) = r {
                    panic!("phase {phase}: {e}");
                }
                return;
            }
            assert!(Instant::now() < deadline, "phase {phase} timed out");
            std::thread::sleep(Duration::from_millis(5));
        }
    }

    // --- the script ---

    /// Anything that goes wrong becomes a message the main thread can panic with.
    struct Fail(String);

    impl From<Status> for Fail {
        fn from(s: Status) -> Fail {
            Fail(format!("{s:?}"))
        }
    }

    /// Report a read by length rather than dumping thousands of bytes into a panic.
    fn brief(r: &Result<Vec<u8>, Status>) -> String {
        match r {
            Ok(v) => format!("Ok({} bytes)", v.len()),
            Err(s) => format!("{s:?}"),
        }
    }

    macro_rules! check {
        ($cond:expr, $($arg:tt)*) => {
            if !$cond {
                return Err(Fail(format!($($arg)*)));
            }
        };
    }

    /// One universe holds every extent here; the allocator is not about partitioning.
    const UNIVERSE: u32 = 1;

    /// Extent bases in that universe's LBA space, in the order `config_text` lays them
    /// out. The huge extents are 1024-aligned because a 4 MiB page spans 1024 blocks.
    const LWW: u64 = 0;
    const OCC: u64 = 4096;
    const IMM: u64 = 4352;
    const BIG: u64 = 5120;
    /// Only exists once the device has been grown for it.
    const GREW: u64 = 7168;
    const GREW_BIG: u64 = 15360;

    /// The LWW page whose bytes phase 2 rots; later phases must not expect it served.
    const ROTTED: u64 = 1;

    /// Page `p` of the 4 KiB extent based at `base`.
    fn at(base: u64, p: u64) -> GlobalAddr {
        GlobalAddr::new(UNIVERSE, base + p)
    }

    /// Page `p` of the 4 MiB extent at `base`, named by the first of its 1024 blocks.
    fn big_at(base: u64, p: u64) -> GlobalAddr {
        GlobalAddr::new(UNIVERSE, base + p * crate::config::HUGE_BLOCKS)
    }

    async fn script(a: &'static Allocator, phase: usize) -> Result<(), Fail> {
        match phase {
            0 => first_boot(a).await,
            1 => restart(a).await,
            2 => grown(a).await,
            3 => corrupted(a).await,
            4 => quarantined(a, false).await,
            _ => quarantined(a, true).await,
        }
    }

    async fn first_boot(a: &'static Allocator) -> Result<(), Fail> {
        // Spread over enough pages that several groups, and so several cores, are hit.
        for p in 0..64u64 {
            put_small(a, at(LWW, p), &pattern(p as u8, SMALL)).await?;
        }
        for p in 0..64u64 {
            let got = get_small(a, at(LWW, p)).await?;
            check!(
                got == pattern(p as u8, SMALL),
                "lww page {p} read back wrong"
            );
        }

        // A page never written is a hole, which is not a page that fails to read.
        let r = get_small(a, at(LWW, 1000)).await;
        check!(
            r == Err(Status::Hole),
            "unwritten page should be a hole, got {}",
            brief(&r)
        );

        // Overwriting bumps the version; the page itself moves to a new slot underneath.
        let v0 = put_small(a, at(LWW, 0), &pattern(0xa5, SMALL)).await?;
        let v1 = put_small(a, at(LWW, 0), &pattern(0x5a, SMALL)).await?;
        check!(v1 > v0, "lww version must advance: {v0} then {v1}");
        let got = get_small(a, at(LWW, 0)).await?;
        check!(got == pattern(0x5a, SMALL), "lww overwrite lost");

        // OCC: a write is accepted only against a version this node has read.
        let addr = at(OCC, 7);
        let r = put_small(a, addr, &pattern(1, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "occ write with no prior read must conflict, got {r:?}"
        );
        let r = get_small(a, addr).await;
        check!(
            r == Err(Status::Hole),
            "occ page starts as a hole, got {}",
            brief(&r)
        );
        put_small(a, addr, &pattern(1, SMALL)).await?;
        // That write left the observation stale, so the next one has to read again.
        let r = put_small(a, addr, &pattern(2, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "occ write on a stale observation must conflict, got {r:?}"
        );
        get_small(a, addr).await?;
        put_small(a, addr, &pattern(2, SMALL)).await?;
        let got = get_small(a, addr).await?;
        check!(got == pattern(2, SMALL), "occ overwrite lost");

        // Immutable, 4 KiB: fill once, refuse the second.
        let addr = at(IMM, 3);
        put_small(a, addr, &pattern(9, SMALL)).await?;
        let r = put_small(a, addr, &pattern(10, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "immutable refill must conflict, got {r:?}"
        );
        // A trim leaves a tombstone: reads as a hole, but the entry survives and blocks
        // a refill.
        let dead = at(IMM, 4);
        put_small(a, dead, &pattern(11, SMALL)).await?;
        a.accept_trim(
            dead,
            3 * server::config().tombstone_epoch_of(dead.0) + 1,
            Ballot::ZERO,
        )
        .await?;
        let r = get_small(a, dead).await;
        check!(
            r == Err(Status::Hole),
            "trimmed page reads as a hole, got {}",
            brief(&r)
        );
        let r = put_small(a, dead, &pattern(12, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "a trimmed immutable page must not be refillable, got {r:?}"
        );

        // Immutable, 4 MiB: the unchecksummed class, written whole.
        let addr = big_at(BIG, 0);
        put_huge(a, addr, &pattern(0x33, HUGE)).await?;
        let got = get_huge(a, addr).await?;
        check!(got == pattern(0x33, HUGE), "huge page read back wrong");
        let r = put_huge(a, addr, &pattern(0x44, HUGE)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "huge refill must conflict, got {r:?}"
        );
        let r = get_huge(a, big_at(BIG, 1)).await;
        check!(
            r == Err(Status::Hole),
            "unwritten huge page is a hole, got {}",
            brief(&r)
        );

        // Holes are served by reading the format-time zero region, so nothing memsets.
        let mut pb = PoolBuf::alloc(HUGE).await;
        a.read_zeroes(pb.buf()).await?;
        check!(pb.iter().all(|b| *b == 0), "zero region is not zero");
        let _ = &mut pb;
        Ok(())
    }

    async fn restart(a: &'static Allocator) -> Result<(), Fail> {
        // Before any read here: the OCC ring is volatile, so a write conflicts until
        // the page is reread.
        let r = put_small(a, at(OCC, 7), &pattern(3, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "occ pool must come up empty, got {r:?}"
        );

        // No journal, no replay: everything below came back from the metadata scan alone.
        let got = get_small(a, at(LWW, 0)).await?;
        check!(
            got == pattern(0x5a, SMALL),
            "lww page 0 lost across restart"
        );
        for p in 1..64u64 {
            let got = get_small(a, at(LWW, p)).await?;
            check!(got == pattern(p as u8, SMALL), "lww page {p} lost");
        }
        let got = get_small(a, at(OCC, 7)).await?;
        check!(got == pattern(2, SMALL), "occ page lost");
        let got = get_small(a, at(IMM, 3)).await?;
        check!(got == pattern(9, SMALL), "immutable page lost");
        let got = get_huge(a, big_at(BIG, 0)).await?;
        check!(got == pattern(0x33, HUGE), "huge page lost");

        // The tombstone survived too: hole versus trim is durable, not just in-process.
        let dead = at(IMM, 4);
        let r = get_small(a, dead).await;
        check!(
            r == Err(Status::Hole),
            "trimmed page should read as a hole, got {}",
            brief(&r)
        );
        let r = put_small(a, dead, &pattern(12, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "tombstone did not survive restart, got {r:?}"
        );
        Ok(())
    }

    /// The device grew under the allocator between boots. Nothing already on it moved, so
    /// this re-runs the durability checks and then uses the appended slots.
    async fn grown(a: &'static Allocator) -> Result<(), Fail> {
        restart(a).await?;

        // The config that did not fit now does.
        let short = layout::shortfall(&a.geometry(), &server::config());
        check!(short == 0, "growth left {short} pages unbacked");

        // Both appended runs hand out slots like any other, including the huge class,
        // whose data has to stay 4 MiB aligned across the join.
        for p in 0..64u64 {
            put_small(a, at(GREW, p), &pattern(p as u8 ^ 0x77, SMALL)).await?;
        }
        for p in 0..64u64 {
            let got = get_small(a, at(GREW, p)).await?;
            check!(
                got == pattern(p as u8 ^ 0x77, SMALL),
                "grown page {p} read back wrong"
            );
        }
        let big = big_at(GREW_BIG, 0);
        put_huge(a, big, &pattern(0x44, HUGE)).await?;
        let got = get_huge(a, big).await?;
        check!(
            got == pattern(0x44, HUGE),
            "grown huge page read back wrong"
        );
        Ok(())
    }

    async fn corrupted(a: &'static Allocator) -> Result<(), Fail> {
        // The asymmetry: the 4 KiB page is refused outright...
        let r = get_small(a, at(LWW, ROTTED)).await;
        check!(
            r == Err(Status::Missing),
            "a 4 KiB page failing its checksum must never be served, got {}",
            brief(&r)
        );
        // ...and the 4 MiB page is handed back wrong, silently: no checksum by design.
        let got = get_huge(a, big_at(BIG, 0)).await?;
        check!(
            got != pattern(0x33, HUGE),
            "the huge page was supposed to be damaged"
        );

        // Damage is contained to the page: its neighbours are untouched.
        let got = get_small(a, at(LWW, 0)).await?;
        check!(got == pattern(0x5a, SMALL), "neighbour page damaged");
        let got = get_small(a, at(LWW, 2)).await?;
        check!(got == pattern(2, SMALL), "neighbour page damaged");
        Ok(())
    }

    /// Both copies of one metadata block are gone, so its entries are gone and nothing
    /// names the pages they described. `peers` says whether consensus has somewhere to
    /// heal from: with peers a miss on that shard is `Missing`, never served and never a
    /// vote; without them it stays a silent hole, the single-node limitation.
    async fn quarantined(a: &'static Allocator, peers: bool) -> Result<(), Fail> {
        let want = if peers { Status::Missing } else { Status::Hole };
        let lost = LOST.lock().unwrap().clone();
        check!(!lost.is_empty(), "the wrecked mblock held no pages");
        for &addr in &lost {
            let r = get_small(a, GlobalAddr(addr)).await;
            check!(
                r == Err(want),
                "lost page {addr:#x}: want {want:?}, got {}",
                brief(&r)
            );
        }

        // Damage is contained. Every page the scan still indexes reads back, whatever
        // shard it is on, and the other class is untouched.
        let mut served = 0;
        for p in 0..64u64 {
            let addr = at(LWW, p);
            if p == ROTTED || lost.contains(&addr.0) {
                continue;
            }
            let got = get_small(a, addr)
                .await
                .map_err(|e| Fail(format!("lww page {p} ({:#x}) unreadable: {e:?}", addr.0)))?;
            check!(
                got == pattern(p as u8, SMALL),
                "lww page {p} damaged by an unrelated mblock"
            );
            served += 1;
        }
        check!(served > 0, "no page survived");
        let got = get_huge(a, big_at(BIG, 0)).await;
        check!(
            got.is_ok(),
            "the huge class shares nothing with the small one"
        );

        // A page never written is indistinguishable from one whose entry was in the lost
        // block, so it degrades with it. Spread wide enough to hit the affected shard.
        let mut degraded = 0;
        for p in 2000..2064u64 {
            let r = get_small(a, at(LWW, p)).await;
            check!(
                matches!(r, Err(Status::Hole) | Err(Status::Missing)),
                "unwritten page {p} served bytes, got {}",
                brief(&r)
            );
            degraded += (r == Err(Status::Missing)) as usize;
        }
        check!(
            (degraded > 0) == peers,
            "unwritten pages degraded on {degraded} of 64 addresses, peers={peers}"
        );
        Ok(())
    }

    // --- page helpers ---

    async fn put_small(
        a: &'static Allocator,
        addr: GlobalAddr,
        data: &[u8],
    ) -> Result<u64, Status> {
        let mut pb = PoolBuf::alloc(SMALL).await;
        pb.copy_from_slice(data);
        let g = a.guard(addr).await?;
        a.accept_small(addr, g, Ballot::ZERO, &pb).await
    }

    async fn get_small(a: &'static Allocator, addr: GlobalAddr) -> Result<Vec<u8>, Status> {
        let mut pb = PoolBuf::alloc(SMALL).await;
        let v = a.read_small(addr, &mut pb).await;
        // Only the node whose device served a read may record the OCC observation, so the
        // allocator never does: `Paxos::read` records in production, the caller here.
        match v {
            Ok(r) => a.observed(addr, r.version).await,
            Err(Status::Hole) => a.observed(addr, 0).await,
            Err(_) => {}
        }
        v?;
        Ok(pb.to_vec())
    }

    async fn put_huge(a: &'static Allocator, addr: GlobalAddr, data: &[u8]) -> Result<u64, Status> {
        let mut pb = PoolBuf::alloc(HUGE).await;
        pb.copy_from_slice(data);
        let g = a.guard(addr).await?;
        a.accept_huge(addr, g, Ballot::ZERO, pb.buf()).await
    }

    async fn get_huge(a: &'static Allocator, addr: GlobalAddr) -> Result<Vec<u8>, Status> {
        let pb = PoolBuf::alloc(HUGE).await;
        a.read_huge(addr, 0, pb.buf()).await?;
        Ok(pb.to_vec())
    }

    fn pattern(seed: u8, len: usize) -> Vec<u8> {
        (0..len)
            .map(|i| seed ^ (i as u8).wrapping_mul(31))
            .collect()
    }

    // --- store ---

    /// Eight groups so addresses spread over workers and exercise the cross-core write
    /// path. All name the same three nodes because this node must own every address it is
    /// asked for; a wider zone would leave it an uneven share.
    fn config_text(dev: &Path) -> String {
        sized(dev, DEV_BYTES)
    }

    fn sized(dev: &Path, bytes: u64) -> String {
        format!(
            "
            generation 1
            node id=1 zone=1 store={} size={bytes}
            universe 1 fabric_device_id=120
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            group 1 2 3
            extent id=1 base=0 pages=4096 kind=lww zone=1
            extent id=2 base=4096 pages=256 kind=occ zone=1
            extent id=3 base=4352 pages=256 kind=immutable zone=1
            extent id=4 base=5120 pages=2 kind=immutable_4m zone=1
            ",
            dev.display()
        )
    }

    /// Where the allocator put a page, read from the metadata region, so the test can rot
    /// one page's bytes behind its back.
    fn slot_of(dev: &Path, class: Class, addr: GlobalAddr) -> Option<u32> {
        let geo = layout::read_geometry(dev).unwrap();
        let f = layout::open_direct(dev, false).unwrap();
        let mut buf = layout::Aligned::new(layout::MBLOCK);
        for id in 0..geo.mblocks(class) as u32 {
            // Take whichever copy is current, the same way startup does.
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

    /// A config that names a peer, so the allocator has somewhere to heal from.
    fn config_peered(dev: &Path) -> String {
        format!(
            "{}\n            peer id=2 device={}\n",
            config_text(dev),
            dev.display()
        )
    }

    /// Two more extents than the store was formatted for, one per class, so `grow` must
    /// append a run to each; they do not fit the first size, so that rises too.
    fn config_grown(dev: &Path) -> String {
        grown_at(dev, GROWN_BYTES)
    }

    fn grown_at(dev: &Path, bytes: u64) -> String {
        format!(
            "{}
            extent id=5 base=7168 pages=8192 kind=lww zone=1
            extent id=6 base=15360 pages=200 kind=immutable_4m zone=1
            ",
            sized(dev, bytes)
        )
    }

    /// The mblock holding `addr`'s entry and every live address in it, about to be lost.
    fn mblock_of(dev: &Path, class: Class, addr: GlobalAddr) -> Option<(u32, Vec<u64>)> {
        let geo = layout::read_geometry(dev).unwrap();
        let f = layout::open_direct(dev, false).unwrap();
        let mut buf = layout::Aligned::new(layout::MBLOCK);
        for id in 0..geo.mblocks(class) as u32 {
            // Take whichever copy is current, as startup does: the stale copy names a
            // different set of pages, and wrecking the block loses the current set anyway.
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
            let live: Vec<u64> = (0..class.k())
                .map(|i| layout::get_entry(&raw, class, i))
                .filter(|e| e.addr != 0 && e.state == State::Live)
                .map(|e| e.addr)
                .collect();
            if live.contains(&addr.0) {
                return Some((id, live));
            }
        }
        None
    }

    /// Rot both copies, the only thing that quarantines: one bad copy is a lost write and
    /// the scan falls back to the other.
    fn wreck_mblock(dev: &Path, class: Class, id: u32) {
        let geo = layout::read_geometry(dev).unwrap();
        for copy in 0..2u8 {
            flip_byte(dev, geo.mblock_off(class, id, copy) + 64);
        }
    }

    fn flip_byte(dev: &Path, off: u64) {
        let f = layout::open_direct(dev, true).unwrap();
        let mut buf = layout::Aligned::new(SMALL);
        let page = off / SMALL as u64 * SMALL as u64;
        layout::read_at(&f, buf.as_mut(), page).unwrap();
        buf.as_mut()[(off % SMALL as u64) as usize] ^= 0xff;
        layout::write_at(&f, buf.as_ref(), page).unwrap();
    }

    /// Needs the real kernel seams, which `sim` replaces.
    #[cfg(not(feature = "sim"))]
    #[test]
    fn allocator() {
        let _only = runtime::exclusive();
        // The runtime needs the ublk control node, which requires both root and a kernel
        // carrying `ublk_drv`; probing the node covers both conditions.
        if std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open("/dev/ublk-control")
            .is_err()
        {
            eprintln!("skipping: reading the ublk feature set needs /dev/ublk-control");
            return;
        }
        let dev = std::env::temp_dir().join(IMG);
        let _ = std::fs::remove_file(&dev);
        let cfg = Config::parse(&config_text(&dev)).unwrap();
        cfg.validate().unwrap();
        layout::size_if_needed(&dev, &cfg).unwrap();
        assert_eq!(std::fs::metadata(&dev).unwrap().len(), DEV_BYTES);
        layout::format(&dev, &cfg).unwrap();

        run(&dev, 0);
        run(&dev, 1);

        // The same extra capacity with no room in the store: valid config, and the layout
        // refuses, naming the store size. Nothing written, so the next step grows it.
        let cramped = Config::parse(&grown_at(&dev, DEV_BYTES)).unwrap();
        cramped.validate().unwrap();
        let e = layout::grow_if_needed(&dev, &cramped).expect_err("no room to append");
        assert!(
            e.to_string().contains("node.store.size_bytes"),
            "says what ran out: {e}"
        );

        // Add capacity the store was not formatted for, as `serve` does, then come back up.
        let grown_cfg = Config::parse(&config_grown(&dev)).unwrap();
        grown_cfg.validate().unwrap();
        layout::grow_if_needed(&dev, &grown_cfg).unwrap();
        assert_eq!(std::fs::metadata(&dev).unwrap().len(), GROWN_BYTES);
        run_with(&dev, 2, &config_grown(&dev));
        // Asking again for what it already has is a no-op.
        layout::grow_if_needed(&dev, &grown_cfg).unwrap();
        assert_eq!(std::fs::metadata(&dev).unwrap().len(), GROWN_BYTES);

        // Rot one byte of one page in each class, then bring it back up.
        let geo = layout::read_geometry(&dev).unwrap();
        let s = slot_of(&dev, Class::Small, at(LWW, ROTTED)).expect("small page placed");
        let h = slot_of(&dev, Class::Huge, big_at(BIG, 0)).expect("huge page placed");
        flip_byte(&dev, geo.slot_off(Class::Small, s) + 17);
        flip_byte(&dev, geo.slot_off(Class::Huge, h) + 17);
        run(&dev, 3);

        // Now lose a whole metadata block: both copies, so the entries it held are
        // unrecoverable locally and the pages they named have no other record.
        let (id, live) = mblock_of(&dev, Class::Small, at(LWW, 0)).expect("mblock");
        *LOST.lock().unwrap() = live
            .into_iter()
            .filter(|a| {
                crate::config::universe_of(*a) == UNIVERSE && crate::config::lba_of(*a) < 4096
            })
            .collect();
        wreck_mblock(&dev, Class::Small, id);
        run(&dev, 4);
        run_with(&dev, 5, &config_peered(&dev));

        let _ = std::fs::remove_file(&dev);
    }
}
