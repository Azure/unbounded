//! The page allocator.

pub mod layout;
mod shard;

use std::cell::RefCell;
use std::future::Future;
use std::rc::Rc;
use std::task::Waker;
use std::time::Instant;

use crate::config::{Config, GroupId, Kind};
use crate::paxos::heal::{self, Tuple};
use crate::paxos::{Ballot, Register};
use crate::runtime::PoolBuf;
use crate::runtime::{self, CoreId, Disk, Durability};
use crate::server::{Server, Worker};

use self::layout::{Class, Geometry, MBLOCK};
use shard::Ticket;
pub use shard::{GlobalAddr, Pressure, Status};
use shard::{Lookup, Maps, Shape, Shard, Staged};

// TODO: What is this?!
macro_rules! maps {
    ($cfg:expr, $m:ident) => {
        let cfg = $cfg;
        let gof = |a: u64, k: $crate::config::Kind| cfg.group_of(a, k);
        let xof = |a: u64| {
            cfg.extent_at(a)
                .map(|e| (e.id, e.tombstone_epoch, e.guard()))
        };
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

mod commit;
mod startup;

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

/// DRAM cost of one resident block: mblock entry plus its share of the index.
/// `config::validate` refuses a working set over `policy.max_index_bytes`, avoiding OOM.
pub const INDEX_BYTES_PER_BLOCK: u64 = 52;

/// DRAM cost of one mutable read record: map entry, share of the table, place in pool order.
const OCC_BYTES_PER_RECORD: u64 = 48;

// ------------------------------------------------------------------------- allocator

/// One worker core's state; the decisions live in `Shard` (`shard.rs`).
struct Core {
    shard: Shard,
    /// Woken by every flush completion on this core; waiters re-check their own condition.
    waiters: Vec<Waker>,
    /// 4 KiB mblock serialization buffer per class, pre-held by `tick` so flush never awaits.
    staging: [Option<PoolBuf>; 2],
    /// Committers parked behind a flush, by class.
    commit_parks: [u64; 2],
    /// Time metadata flushes were in flight, by class.
    flush_busy_us: [u64; 2],
    flush_started: [Option<Instant>; 2],
}

pub struct Allocator {
    disk: Disk,
    geo: Geometry,
    cores: usize,
    /// Consensus side state as the superblock held it at startup: `promised_term` per
    /// group and the seal table. `paxos` takes it from here and owns it thereafter.
    boot: layout::Consensus,
    /// Metadata blocks lost at startup, surfaced as a health metric.
    pub quarantined: usize,
}

/// One worker's share of the allocator, living in that worker's [`crate::server::CoreState`].
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
        }))
    }
}

/// This core's share.
fn here<T>(worker: &Worker, f: impl FnOnce(&mut Core) -> T) -> T {
    f(&mut worker.core().alloc.0.borrow_mut())
}

/// The decision-making half of it, which is most of what callers want.
fn shard<T>(worker: &Worker, f: impl FnOnce(&mut Shard) -> T) -> T {
    here(worker, |c| f(&mut c.shard))
}

/// `core`'s share, as a transaction that core runs.
///
/// Synchronous by construction, which is what makes it a transaction: a reservation, a
/// lookup or a register read is one visit to the owning worker, resolved inside the drain
/// that delivered it, rather than a task parked there waiting to be polled again.
fn at<T, F>(core: CoreId, f: F) -> impl Future<Output = T>
where
    F: FnOnce(&Worker, &mut Core) -> T + Send + 'static,
    T: Send + 'static,
{
    runtime::to::<Server, _, _>(core, move |_, worker| {
        f(worker, &mut worker.core().alloc.0.borrow_mut())
    })
}

/// Cores that participate in a class's index sharding. Mblocks are striped `id % cores`
/// and a core allocates only from its own stripe, so capping the width at the mblock
/// count keeps every owning core able to allocate. A small slab has fewer mblocks than
/// workers.
fn shards_for(cores: usize, geo: &Geometry, class: Class) -> usize {
    cores.min(geo.mblocks(class).max(1) as usize)
}

/// This core's share of the mutable observation pool. Per core because a record is only touched by its
/// owning core and a shared pool would need a hot-path lock. Cost: a dropped record
/// turns a success into a conflict, which is a retry, never a wrong answer.
fn occ_per_core(bytes: u64, cores: usize) -> usize {
    (bytes / OCC_BYTES_PER_RECORD / cores.max(1) as u64).max(1) as usize
}

/// The shape the device's geometry implies; `shard::model` supplies its own numbers.
fn shape_of(geo: &Geometry, cfg: &Config, cores: usize) -> Shape {
    Shape {
        cores: cores as u32,
        k: [Class::Mutable.k(), Class::Immutable.k()],
        mblocks: [geo.mblocks(Class::Mutable), geo.mblocks(Class::Immutable)],
        expect: [
            geo.slots(Class::Mutable) / shards_for(cores, geo, Class::Mutable) as u64,
            geo.slots(Class::Immutable) / shards_for(cores, geo, Class::Immutable) as u64,
        ],
        shards_for: [
            shards_for(cores, geo, Class::Mutable),
            shards_for(cores, geo, Class::Immutable),
        ],
        occ: occ_per_core(cfg.occ_bytes(), cores),
        recheck: true,
    }
}

/// Group-level operations name a slab with one bit: the mutable class or the immutable one.
fn class_of(immutable: bool) -> Class {
    if immutable { Class::Immutable } else { Class::Mutable }
}

impl Allocator {
    /// The core that owns an address's index shard, and so allocates for it. Reuses the
    /// consensus group mapping so the lookup rides the hop consensus already makes.
    fn owner(&self, worker: &Worker, addr: GlobalAddr, class: Class) -> CoreId {
        let i = worker.config().group_of(addr.0, class.kind()).index() as usize
            % shards_for(self.cores, &self.geo, class);
        CoreId::of(i)
    }

    /// The extent's block kind, class and tombstone epoch, from one lookup.
    fn extent(&self, worker: &Worker, addr: GlobalAddr) -> Option<(Kind, Class, u64)> {
        let cfg = worker.config();
        let e = cfg.extent_at(addr.0)?;
        Some((e.guard(), Class::of(e.guard()), e.tombstone_epoch as u64))
    }

    /// The extent's block kind.
    pub fn kind_of(&self, worker: &Worker, addr: GlobalAddr) -> Result<Kind, Status> {
        let (kind, _, _) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        Ok(kind)
    }

    /// The core owning an address. The cache shards by this too, so its lookups are free.
    pub fn owner_core(&self, worker: &Worker, addr: GlobalAddr) -> Result<CoreId, Status> {
        let (_, class, _) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        Ok(self.owner(worker, addr, class))
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
    pub fn pressure(&self, worker: &Worker) -> Pressure {
        shard(worker, |sh| sh.pressure())
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
    pub fn capacity(&self, worker: &Worker) -> [(u64, u64); 2] {
        shard(worker, |sh| sh.capacity())
    }

    /// Live and tombstoned pages per extent in this core's shards,
    /// `(extent, live, tombstones)`. Per core like `capacity`; the exporter sums them.
    pub fn census(&self, worker: &Worker) -> Vec<(u32, u64, u64)> {
        shard(worker, |sh| sh.census())
    }

    pub fn cores(&self) -> usize {
        self.cores
    }

    // ------------------------------------------------------------------ reservations

    /// Guard check plus free-list pop, all on the owning core. Synchronous: no IO is
    /// issued, so a refusal costs nothing. A present `guard` is the collision detector
    /// and the whole type check: OCC and Immutable differ only in which version the
    /// proposer presented. `None` asks the shard to derive it from the local row.
    #[allow(clippy::too_many_arguments)]
    fn reserve_in(
        &self,
        worker: &Worker,
        sh: &mut Shard,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        guard: Option<u64>,
        ballot: Ballot,
    ) -> Result<Ticket, Status> {
        let epoch = worker.config().tombstone_epoch_of(addr.0);
        sh.reserve(addr, kind, class, guard, ballot, epoch)
    }

    /// The owning core's half of a commit: install the entry, retire the previous slot,
    /// mark the mblock dirty. Returns what `flush_until` must reach for durability.
    fn stage_in(
        &self,
        worker: &Worker,
        sh: &mut Shard,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        crc: u32,
    ) -> Result<Option<Staged>, Status> {
        let kind = self.extent(worker, addr).ok_or(Status::Unmapped)?.0;
        maps!(worker.config(), m);
        sh.stage(addr, kind, class, t, crc, &m)
    }

    /// Undo a reservation whose data write failed, so the slot is not leaked.
    fn unreserve_in(&self, worker: &Worker, sh: &mut Shard, class: Class, t: Ticket) {
        maps!(worker.config(), m);
        sh.unreserve(class, t, &m);
    }

    // -------------------------------------------------------------------- write paths

    /// The proposer's own leg of an accept, stopped short of the register: page durable,
    /// slot held, but the node reads as before, so a lost proposal leaves no trace here.
    pub async fn begin_block(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        guard: u64,
        ballot: Ballot,
        page: &PoolBuf,
    ) -> Result<Pending, Status> {
        let (kind, class, _) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        if page.len() != layout::SMALL_PAGE as usize {
            return Err(Status::Unmapped);
        }
        let owner = self.owner(worker, addr, class);
        let t = at(owner, move |worker, c| {
            self.reserve_in(worker, &mut c.shard, addr, kind, class, Some(guard), ballot)
        })
        .await?;
        let (ticket, crc) = self.write_page(worker, addr, class, t, page).await?;
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
        worker: &Worker,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        page: &PoolBuf,
    ) -> Result<(Ticket, u32), Status> {
        // Only the mutable class carries an authoritative data checksum. An immutable
        // block is write-once, so its bytes are protected by ordering and by the
        // lookup-read-lookup in `read_block`, not by a CRC nobody would ever recompute.
        let crc = if class.checksummed() {
            layout::page_crc(addr.0, t.version, page)
        } else {
            0
        };
        let off = self.geo.slot_off(class, t.slot);
        if self
            .disk
            .write(off, page.buf(), Durability::Durable)
            .await
            .is_err()
        {
            let owner = self.owner(worker, addr, class);
            at(owner, move |worker, c| {
                self.unreserve_in(worker, &mut c.shard, class, t)
            })
            .await;
            return Err(Status::Io);
        }
        Ok((t, crc))
    }

    /// Install a staged entry, now that a quorum holds the value. A refusal means our row
    /// moved while the peers were answering, so the version is not ours to report.
    pub async fn finish(&'static self, worker: &Worker, p: Pending) -> Result<u64, Status> {
        let (addr, class, t, crc) = (p.addr, p.class, p.ticket, p.crc);
        let owner = self.owner(worker, addr, class);
        let version = t.version;
        let done = runtime::to_async_with::<Server, _, _, _>(owner, move |worker| {
            Box::pin(self.commit(worker, addr, class, t, crc))
        })
        .await?;
        if done {
            Ok(version)
        } else {
            Err(Status::Conflict { current: 0 })
        }
    }

    /// Give the slot back: this proposal never reached a quorum.
    pub async fn abandon(&'static self, worker: &Worker, p: Pending) {
        let (addr, class, t) = (p.addr, p.class, p.ticket);
        let owner = self.owner(worker, addr, class);
        at(owner, move |worker, c| {
            self.unreserve_in(worker, &mut c.shard, class, t)
        })
        .await;
    }

    /// The member side of consensus: apply this page iff the guard matches. An error
    /// leaves the register untouched, so a version read off two acceptors has bytes.
    pub async fn accept_block(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        guard: u64,
        ballot: Ballot,
        page: &PoolBuf,
    ) -> Result<u64, Status> {
        // Losing the row is a refusal, not a success: an acceptor that acks a value it
        // did not install lends its vote to a quorum that does not exist.
        self.put_block(worker, addr, Some(guard), ballot, page)
            .await?
            .ok_or(Status::Conflict { current: 0 })
    }

    async fn put_block(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        guard: Option<u64>,
        ballot: Ballot,
        page: &PoolBuf,
    ) -> Result<Option<u64>, Status> {
        let (kind, class, _) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        if page.len() != layout::SMALL_PAGE as usize {
            return Err(Status::Unmapped);
        }
        let owner = self.owner(worker, addr, class);
        let t = at(owner, move |worker, c| {
            self.reserve_in(worker, &mut c.shard, addr, kind, class, guard, ballot)
        })
        .await?;
        self.finish_block(worker, addr, class, t, page).await
    }

    /// Data first, then the entry that names it; the guarded and unguarded paths share
    /// this. The ordering is about the register, not torn reads (the checksum catches
    /// those): an acceptor that answered no must still read as before, or two failed data
    /// writes leave a version that looks chosen to later recoveries and nobody can serve.
    async fn finish_block(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        page: &PoolBuf,
    ) -> Result<Option<u64>, Status> {
        let (t, crc) = self.write_page(worker, addr, class, t, page).await?;
        let owner = self.owner(worker, addr, class);
        let version = t.version;
        let done = runtime::to_async_with::<Server, _, _, _>(owner, move |worker| {
            Box::pin(self.commit(worker, addr, class, t, crc))
        })
        .await?;
        Ok(done.then_some(version))
    }

    // -------------------------------------------------------------- consensus surface

    /// The register as this node holds it, with no data read. A page never seen sits at
    /// version zero, or `3 * epoch` for an Immutable extent, and consensus needs that as
    /// a vote. A page we should hold but cannot serve reports `Missing`, not a vote.
    pub async fn register(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
    ) -> Result<Register, Status> {
        let owner = self.owner_core(worker, addr)?;
        at(owner, move |worker, c| {
            self.register_of(worker, &mut c.shard, addr)
        })
        .await
    }

    /// [`register`](Self::register) without the hop, for a caller already on the owning
    /// core. `paxos` uses it to bump the cache's sketch and read the register in one hop.
    pub fn register_local(&self, worker: &Worker, addr: GlobalAddr) -> Result<Register, Status> {
        shard(worker, |sh| self.register_of(worker, sh, addr))
    }

    /// [`register`](Self::register) against a shard already open.
    fn register_of(
        &self,
        worker: &Worker,
        sh: &mut Shard,
        addr: GlobalAddr,
    ) -> Result<Register, Status> {
        let (kind, class, epoch) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        debug_assert_eq!(runtime::core(), self.owner(worker, addr, class));
        sh.register_of(addr, kind, class, epoch)
    }

    /// The guard a proposer on this node should present. The OCC read ring is consulted
    /// here, which is why acceptors need no read-tracking state of their own.
    pub async fn guard(&'static self, worker: &Worker, addr: GlobalAddr) -> Result<u64, Status> {
        let (kind, class, epoch) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        at(self.owner(worker, addr, class), move |_, c| {
            c.shard.guard_of(addr, kind, class, epoch)
        })
        .await
    }

    /// Record a read this node's own ublk device served: the only thing feeding the OCC
    /// ring. Serving a peer is not a read of ours.
    pub async fn observed(&'static self, worker: &Worker, addr: GlobalAddr, version: u64) {
        let Some((kind, class, _)) = self.extent(worker, addr) else {
            return;
        };
        at(self.owner(worker, addr, class), move |_, c| {
            c.shard.observed(addr, kind, version);
        })
        .await
    }

    /// The unguarded apply-if-newer write: repair and `learn`, one operation with two
    /// provenances. Legal only underneath a prepare, or into a migration destination.
    /// `Ok(false)` means what we hold is at least as new, which makes migration streams
    /// commutative and a repeated repair free. `replace_equal` also rewrites a row held
    /// at exactly `r`: how a repair replaces bytes that failed checksum at that register.
    pub async fn learn_block(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        r: Register,
        page: &PoolBuf,
        replace_equal: bool,
    ) -> Result<bool, Status> {
        let (_, class, _) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        if page.len() != layout::SMALL_PAGE as usize {
            return Err(Status::Unmapped);
        }
        let Some(t) = self
            .reserve_unguarded(worker, addr, class, r, replace_equal)
            .await?
        else {
            return Ok(false);
        };
        if replace_equal {
            let (t, crc) = self.write_page(worker, addr, class, t, page).await?;
            let owner = self.owner(worker, addr, class);
            return runtime::to_async_with::<Server, _, _, _>(owner, move |worker| {
                Box::pin(self.commit_replace_equal(worker, addr, class, t, crc))
            })
            .await;
        }
        Ok(self
            .finish_block(worker, addr, class, t, page)
            .await?
            .is_some())
    }

    /// Learn a register with no bytes: an Immutable tombstone. No member has a page to
    /// serve, so the register is the value; a replica missing it diverges at the fill point.
    pub async fn learn_tombstone(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        r: Register,
    ) -> Result<bool, Status> {
        let (_, class, _) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        let Some(t) = self
            .reserve_unguarded(worker, addr, class, r, false)
            .await?
        else {
            return Ok(false);
        };
        let owner = self.owner(worker, addr, class);
        runtime::to_async_with::<Server, _, _, _>(owner, move |worker| {
            Box::pin(self.commit(worker, addr, class, t, 0))
        })
        .await
    }

    async fn reserve_unguarded(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        class: Class,
        r: Register,
        replace_equal: bool,
    ) -> Result<Option<Ticket>, Status> {
        let (kind, _, epoch) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        at(self.owner(worker, addr, class), move |_worker, c| {
            c.shard
                .reserve_unguarded(addr, kind, class, r, epoch, replace_equal)
        })
        .await
    }

    // --------------------------------------------------------------------- read paths

    /// One 4 KiB block read.
    ///
    /// A mutable block is verified against the entry's seeded CRC; a mismatch is never
    /// served, and repair replaces the bytes at the same register.
    ///
    /// An immutable block has no data checksum, so ordering and a second lookup stand in
    /// for one. Nothing held the slot still while those bytes were read: the entry could
    /// have been trimmed, its slot given back, and that slot reserved and written for
    /// some other address. Asking again rules that out. A version only ever moves
    /// forward, so an entry answering with the same slot at the same register is an entry
    /// that never left: had the slot been freed, getting back to this address would have
    /// taken a write, and a write would have taken a higher version. What this cannot
    /// catch is stable rot under an untouched entry, which is the deliberate price of not
    /// checksumming write-once data.
    pub async fn read_block(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        page: &mut PoolBuf,
    ) -> Result<Register, Status> {
        let (kind, class, _) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        if page.len() != layout::SMALL_PAGE as usize {
            return Err(Status::Unmapped);
        }
        let owner = self.owner(worker, addr, class);
        let l = at(owner, move |_, c| c.shard.lookup(addr, kind, class)).await?;
        let off = self.geo.slot_off(class, l.slot);
        self.disk
            .read(off, page.buf())
            .await
            .map_err(|_| Status::Io)?;
        if class.checksummed() {
            if layout::page_crc(addr.0, l.version, page) != l.crc {
                return Err(Status::Missing);
            }
        } else if at(owner, move |_, c| c.shard.lookup(addr, kind, class)).await? != l {
            return Err(Status::Missing);
        }
        Ok(Self::reg_of(&l))
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
        worker: &Worker,
        addr: GlobalAddr,
        guard: u64,
        ballot: Ballot,
    ) -> Result<(), Status> {
        self.trim_at(worker, addr, Some(guard), ballot).await
    }

    async fn trim_at(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        guard: Option<u64>,
        ballot: Ballot,
    ) -> Result<(), Status> {
        let (kind, class, _) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        let owner = self.owner(worker, addr, class);
        // The mblock index is the owner's, so the flush has to happen there too.
        runtime::to_async_with::<Server, _, _, _>(owner, move |worker| async move {
            match self.trim(&worker, addr, kind, class, guard, ballot)? {
                Some((li, need)) => self.flush_until(worker, class, li, need).await,
                None => Ok(()),
            }
        })
        .await
    }

    fn trim(
        &self,
        worker: &Worker,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        guard: Option<u64>,
        ballot: Ballot,
    ) -> Result<Option<(u32, u64)>, Status> {
        let epoch = worker.config().tombstone_epoch_of(addr.0);
        maps!(worker.config(), m);
        shard(worker, |sh| {
            sh.trim(addr, kind, class, guard, ballot, epoch, &m)
        })
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
    pub async fn digests(
        &'static self,
        _worker: &Worker,
        group: GroupId,
        immutable: bool,
    ) -> Box<[u64; heal::BUCKETS]> {
        let class = class_of(immutable);
        at(self.owner_of(group, class), move |_, c| {
            c.shard.digest_vector(class, group)
        })
        .await
    }

    /// Open an enumeration of a group's registers, narrowed by `filter`.
    pub async fn snap_open(
        &'static self,
        worker: Rc<Worker>,
        group: GroupId,
        immutable: bool,
        filter: heal::Filter,
    ) -> Result<Snapshot, Status> {
        let class = class_of(immutable);
        let core = self.owner_of(group, class);
        let id = at(core, move |_, c| {
            let now = runtime::now();
            c.shard
                .snap_start(class, core.index(), immutable, group, filter, now)
                .ok_or(Status::NoSpace)
        })
        .await?;
        Ok(Snapshot {
            alloc: self,
            worker,
            id,
            active: true,
        })
    }

    /// Next chunk. Bounded by entries scanned as well as tuples produced, so a sparse
    /// filter costs more frames but never a long stall on the owning core. `universe` is
    /// the request's namespace, `None` for a local caller; a cursor answers only its own.
    pub async fn snap_next(
        &'static self,
        _worker: &Worker,
        id: u32,
        seq: Option<u8>,
        universe: Option<u32>,
    ) -> Result<(Vec<Tuple>, bool), Status> {
        let (core, immutable, _, _) = heal::snap_parts(id);
        if core >= self.cores {
            return Err(Status::Unmapped);
        }
        let class = class_of(immutable);
        at(CoreId::of(core), move |worker, c| {
            let now = runtime::now();
            maps!(worker.config(), m);
            c.shard.snap_next(class, id, seq, universe, &m, now)
        })
        .await
    }

    /// Close a cursor. Idempotent, and the last one out resumes reclamation.
    pub async fn snap_release(&'static self, _worker: &Worker, id: u32) {
        let (core, immutable, _, _) = heal::snap_parts(id);
        if core >= self.cores {
            return;
        }
        let class = class_of(immutable);
        at(CoreId::of(core), move |_, c| c.shard.snap_stop(class, id)).await
    }

    // ------------------------------------------------------------------------- shed

    /// Groups this core still holds registers for, in one class. Per core like `census`:
    /// a group's registers, and its digests, all live on the one core its id maps to.
    pub fn held_groups(&self, worker: &Worker, immutable: bool) -> Vec<GroupId> {
        shard(worker, |sh| sh.held_groups(class_of(immutable)))
    }

    /// Forget a group this core has been drained of, so it drops out of `held_groups`.
    pub async fn forget_group(&'static self, _worker: &Worker, group: GroupId, immutable: bool) {
        let class = class_of(immutable);
        at(self.owner_of(group, class), move |_, c| {
            c.shard.forget_group(class, group);
        })
        .await
    }

    /// Drop a register this node no longer owns. Nothing checks that: the caller has
    /// confirmed the new holders. Refuses if the register moved, making that stale.
    pub async fn discard(
        &'static self,
        worker: &Worker,
        addr: GlobalAddr,
        version: u64,
    ) -> Result<(), Status> {
        let (_, class, _) = self.extent(worker, addr).ok_or(Status::Unmapped)?;
        let owner = self.owner(worker, addr, class);
        // The mblock index is the owner's, so the flush has to happen there too.
        runtime::to_async_with::<Server, _, _, _>(owner, move |worker| async move {
            maps!(worker.config(), m);
            // Bound before the match: a temporary in the scrutinee would hold the shard
            // borrow across the flush's await.
            let hit = shard(&worker, |sh| sh.discard(addr, class, version, &m));
            match hit {
                Some((li, need)) => self.flush_until(worker, class, li, need).await,
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
    pub fn tick(&self, worker: &Worker, now: Instant) {
        let cfg = worker.config();
        maps!(cfg, m);
        // Nothing to reclaim until the configuration can retire something.
        let reclaimable = cfg.reclaimable();
        here(worker, |c| {
            for class in [Class::Mutable, Class::Immutable] {
                if c.staging[class as usize].is_none() {
                    c.staging[class as usize] = PoolBuf::try_alloc(MBLOCK);
                }
                c.shard.snap_expire(class, now);
                if reclaimable {
                    c.shard.sweep(class, &m);
                }
            }
            c.shard.set_occ(occ_per_core(cfg.occ_bytes(), self.cores));
            c.shard.set_recoverable(cfg.peer_count() > 0);
        });
    }

    /// This core's group-commit counters.
    pub fn local_stats(&self, worker: &Worker) -> Stats {
        here(worker, |c| {
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
    worker: Rc<Worker>,
    id: u32,
    active: bool,
}

impl Snapshot {
    /// The next chunk, and whether it was the last.
    pub async fn next(&self) -> Result<(Vec<Tuple>, bool), Status> {
        self.alloc
            .snap_next(&self.worker, self.id, None, None)
            .await
    }

    /// Close the walk, resuming reclamation once this was the last cursor out.
    pub async fn close(mut self) {
        self.alloc.snap_release(&self.worker, self.id).await;
        self.active = false;
    }

    /// Give the cursor to whoever asked for it over the wire, along with the closing.
    pub fn into_wire(mut self) -> u32 {
        self.active = false;
        self.id
    }
}

impl Drop for Snapshot {
    fn drop(&mut self) {
        if !self.active {
            return;
        }
        let (alloc, worker, id) = (self.alloc, self.worker.clone(), self.id);
        // Detached, because a destructor cannot await. If the slab has no room the cursor
        // waits out its deadline, which is what abandoning one did every time before this.
        let _ = runtime::spawn_local(async move { alloc.snap_release(&worker, id).await });
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
    startup::open(path, disk, cfg, cores)
}

// --- Invariants, for the sim ---

/// What must be true of the allocator between actions, whatever the fabric is doing to
/// it. Sampled by the simulator after every step a fuzzer takes, so a violation is
/// caught at the action that caused it rather than at the read that eventually noticed.
impl Row {
    /// What must hold of one core's share whatever has been done to it. The simulator
    /// samples this between steps; nothing else calls it.
    // The invariant surface a campaign samples after every step it takes. Nothing
    // reaches it yet: a `Cluster` has no way into a node's worker fibers, which is what
    // the fuzz campaign that follows will add.
    #[allow(dead_code)]
    pub(crate) fn invariants(&self) -> Result<(), String> {
        // A borrow held here means a worker is mid-mutation, which only happens if
        // this was called from inside the runtime. Say so rather than panicking.
        let Ok(c) = self.0.try_borrow() else {
            return Err("the shard is borrowed".into());
        };
        c.shard.invariants()
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
    use std::rc::Rc;
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::{Duration, Instant};

    use super::{Allocator, GlobalAddr, Status};
    use crate::config::Config;
    use crate::layout::{self, Class, State};
    use crate::paxos::Ballot;
    use crate::runtime::PoolBuf;
    use crate::runtime::{self, CoreId, Errno, Handler, Request, ResourceBuild};
    use crate::server;

    const IMG: &str = "racer-alloc.img";

    /// Just what the base config plans, so appended runs need a larger store, not slack.
    fn dev_bytes() -> u64 {
        layout::store_floor(4352, 3072)
    }

    /// What the store has to be told to be before the appended runs fit in it.
    fn grown_bytes() -> u64 {
        layout::store_floor(12544, 5120)
    }
    const SMALL: usize = 4096;

    // --- harness ---

    struct Driver;

    fn bare(c: &ResourceBuild, cfg: Config) -> std::io::Result<server::NodeGeneration> {
        server::Node::new()
            .build_generation(c, None, cfg)?
            .ok_or_else(|| std::io::Error::other("initial generation was rejected"))
    }

    static PHASE: AtomicUsize = AtomicUsize::new(0);
    static RESULT: Mutex<Option<Result<(), String>>> = Mutex::new(None);
    static STARTED: AtomicUsize = AtomicUsize::new(0);
    /// Mutable addresses whose entries were in the mblock the test wrecked.
    static LOST: Mutex<Vec<u64>> = Mutex::new(Vec::new());

    impl Handler for Driver {
        type Config = server::NodeGeneration;
        type Worker = server::Worker;

        fn build_worker(
            core: CoreId,
            cfg: std::sync::Arc<server::NodeGeneration>,
            previous: Option<&server::Worker>,
        ) -> server::Worker {
            <server::Server as Handler>::build_worker(core, cfg, previous)
        }

        async fn handle(_worker: Rc<server::Worker>, _req: Request) -> Result<(), Errno> {
            Err(Errno::EOPNOTSUPP)
        }

        fn tick(worker: Rc<server::Worker>, now: Instant) {
            worker.node().alloc().tick(&worker, now);
            // Launch the script once, from core 0 onto core 1, to exercise the cross-core
            // path. `runtime::spawn_local` is core-local, so only a hop carries a task
            // elsewhere. Submission is eager; dropping the reply does not stop the task.
            if runtime::core().index() != 0 || STARTED.swap(1, Ordering::SeqCst) == 1 {
                return;
            }
            let phase = PHASE.load(Ordering::SeqCst);
            drop(runtime::to_async_with::<Driver, _, _, _>(
                CoreId::of(1),
                move |worker| boxed(worker, phase),
            ));
        }
    }

    /// Boxed so the hop's payload and task-slot size limits see a fat pointer.
    fn boxed(worker: Rc<server::Worker>, phase: usize) -> Pin<Box<dyn Future<Output = ()>>> {
        Box::pin(async move {
            let r = script(worker, phase).await.map_err(|f| f.0);
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
        let rt = runtime::start::<Driver>().expect("start");
        rt.update(move |c, _| bare(c, cfg).map(Some))
            .expect("update");

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
    /// out. The immutable ones are 1024-aligned because an immutable block is placed by
    /// the 4 MiB stripe that contains it.
    const MUTABLE: u64 = 0;
    const OCC: u64 = 4096;
    const IMM: u64 = 5120;
    const BIG: u64 = 6144;
    /// Only exists once the device has been grown for it.
    const GREW: u64 = 8192;
    const GREW_BIG: u64 = 17408;

    /// The mutable page whose bytes phase 2 rots; later phases must not expect it served.
    const ROTTED: u64 = 1;

    /// Page `p` of the 4 KiB extent based at `base`.
    fn at(base: u64, p: u64) -> GlobalAddr {
        GlobalAddr::new(UNIVERSE, base + p)
    }

    /// Block `p` of the immutable extent at `base`. `p` counts blocks, not stripes: the
    /// stripe only decides placement, so sibling blocks are independent rows.
    fn big_at(base: u64, p: u64) -> GlobalAddr {
        GlobalAddr::new(UNIVERSE, base + p)
    }

    async fn script(worker: Rc<server::Worker>, phase: usize) -> Result<(), Fail> {
        let a = worker.node().alloc();
        match phase {
            0 => first_boot(&worker, a).await,
            1 => restart(&worker, a).await,
            2 => grown(&worker, a).await,
            3 => corrupted(&worker, a).await,
            4 => quarantined(&worker, a, false).await,
            _ => quarantined(&worker, a, true).await,
        }
    }

    async fn first_boot(worker: &server::Worker, a: &'static Allocator) -> Result<(), Fail> {
        // Spread over enough pages that several groups, and so several cores, are hit.
        for p in 0..64u64 {
            let addr = at(MUTABLE, p);
            let _ = get_small(worker, a, addr).await;
            put_block(worker, a, addr, &pattern(p as u8, SMALL)).await?;
        }
        for p in 0..64u64 {
            let got = get_small(worker, a, at(MUTABLE, p)).await?;
            check!(
                got == pattern(p as u8, SMALL),
                "mutable page {p} read back wrong"
            );
        }

        // A page never written is a hole, which is not a page that fails to read.
        let r = get_small(worker, a, at(MUTABLE, 1000)).await;
        check!(
            r == Err(Status::Hole),
            "unwritten page should be a hole, got {}",
            brief(&r)
        );

        // Overwriting bumps the version; the page itself moves to a new slot underneath.
        let v0 = put_block(worker, a, at(MUTABLE, 0), &pattern(0xa5, SMALL)).await?;
        get_small(worker, a, at(MUTABLE, 0)).await?;
        let v1 = put_block(worker, a, at(MUTABLE, 0), &pattern(0x5a, SMALL)).await?;
        check!(v1 > v0, "mutable version must advance: {v0} then {v1}");
        let got = get_small(worker, a, at(MUTABLE, 0)).await?;
        check!(got == pattern(0x5a, SMALL), "mutable overwrite lost");

        // A page staged in the head of a larger buffer, as a fabric accept does: what is
        // written is the truncated view, and the trailer beyond it never reaches the slot.
        let addr = at(MUTABLE, 100);
        let _ = get_small(worker, a, addr).await;
        let mut staged = PoolBuf::alloc(2 * SMALL).await;
        staged[..SMALL].copy_from_slice(&pattern(0x77, SMALL));
        staged[SMALL..].fill(0xff);
        staged.truncate(SMALL);
        let g = a.guard(worker, addr).await?;
        a.accept_block(worker, addr, g, Ballot::ZERO, &staged)
            .await?;
        let got = get_small(worker, a, addr).await?;
        check!(
            got == pattern(0x77, SMALL),
            "truncated staging page read back wrong"
        );

        // OCC: a write is accepted only against a version this node has read.
        let addr = at(OCC, 7);
        let r = put_block(worker, a, addr, &pattern(1, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "occ write with no prior read must conflict, got {r:?}"
        );
        let r = get_small(worker, a, addr).await;
        check!(
            r == Err(Status::Hole),
            "occ page starts as a hole, got {}",
            brief(&r)
        );
        put_block(worker, a, addr, &pattern(1, SMALL)).await?;
        // That write left the observation stale, so the next one has to read again.
        let r = put_block(worker, a, addr, &pattern(2, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "occ write on a stale observation must conflict, got {r:?}"
        );
        get_small(worker, a, addr).await?;
        put_block(worker, a, addr, &pattern(2, SMALL)).await?;
        let got = get_small(worker, a, addr).await?;
        check!(got == pattern(2, SMALL), "occ overwrite lost");

        // Immutable, 4 KiB: fill once, refuse the second.
        let addr = at(IMM, 3);
        put_block(worker, a, addr, &pattern(9, SMALL)).await?;
        let r = put_block(worker, a, addr, &pattern(10, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "immutable refill must conflict, got {r:?}"
        );
        // A trim leaves a tombstone: reads as a hole, but the entry survives and blocks
        // a refill.
        let dead = at(IMM, 4);
        put_block(worker, a, dead, &pattern(11, SMALL)).await?;
        a.accept_trim(
            worker,
            dead,
            3 * worker.config().tombstone_epoch_of(dead.0) + 1,
            Ballot::ZERO,
        )
        .await?;
        let r = get_small(worker, a, dead).await;
        check!(
            r == Err(Status::Hole),
            "trimmed page reads as a hole, got {}",
            brief(&r)
        );
        let r = put_block(worker, a, dead, &pattern(12, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "a trimmed immutable page must not be refillable, got {r:?}"
        );

        // The unchecksummed class. One block of a stripe is written; the other 1023 are
        // still holes, so a stripe costs only what was put in it.
        let addr = big_at(BIG, 0);
        put_block(worker, a, addr, &pattern(0x33, SMALL)).await?;
        let got = get_small(worker, a, addr).await?;
        check!(got == pattern(0x33, SMALL), "immutable block read back wrong");
        let r = put_block(worker, a, addr, &pattern(0x44, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "immutable refill must conflict, got {r:?}"
        );
        for p in [1u64, 2, 1023] {
            let r = get_small(worker, a, big_at(BIG, p)).await;
            check!(
                r == Err(Status::Hole),
                "block {p} of a written stripe is still a hole, got {}",
                brief(&r)
            );
        }
        // A sibling in the same stripe is its own write-once register.
        let sib = big_at(BIG, 1023);
        put_block(worker, a, sib, &pattern(0x55, SMALL)).await?;
        let got = get_small(worker, a, sib).await?;
        check!(got == pattern(0x55, SMALL), "stripe sibling read back wrong");
        let got = get_small(worker, a, addr).await?;
        check!(
            got == pattern(0x33, SMALL),
            "writing a sibling disturbed its neighbor"
        );
        Ok(())
    }

    async fn restart(worker: &server::Worker, a: &'static Allocator) -> Result<(), Fail> {
        // Before any read here: the OCC ring is volatile, so a write conflicts until
        // the page is reread.
        let r = put_block(worker, a, at(OCC, 7), &pattern(3, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "occ pool must come up empty, got {r:?}"
        );

        // No journal, no replay: everything below came back from the metadata scan alone.
        let got = get_small(worker, a, at(MUTABLE, 0)).await?;
        check!(
            got == pattern(0x5a, SMALL),
            "mutable page 0 lost across restart"
        );
        for p in 1..64u64 {
            let got = get_small(worker, a, at(MUTABLE, p)).await?;
            check!(got == pattern(p as u8, SMALL), "mutable page {p} lost");
        }
        let got = get_small(worker, a, at(OCC, 7)).await?;
        check!(got == pattern(2, SMALL), "occ page lost");
        let got = get_small(worker, a, at(IMM, 3)).await?;
        check!(got == pattern(9, SMALL), "immutable page lost");
        let got = get_small(worker, a, big_at(BIG, 0)).await?;
        check!(got == pattern(0x33, SMALL), "immutable block lost");
        let got = get_small(worker, a, big_at(BIG, 1023)).await?;
        check!(got == pattern(0x55, SMALL), "stripe sibling lost");

        // The tombstone survived too: hole versus trim is durable, not just in-process.
        let dead = at(IMM, 4);
        let r = get_small(worker, a, dead).await;
        check!(
            r == Err(Status::Hole),
            "trimmed page should read as a hole, got {}",
            brief(&r)
        );
        let r = put_block(worker, a, dead, &pattern(12, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "tombstone did not survive restart, got {r:?}"
        );
        Ok(())
    }

    /// The device grew under the allocator between boots. Nothing already on it moved, so
    /// this re-runs the durability checks and then uses the appended slots.
    async fn grown(worker: &server::Worker, a: &'static Allocator) -> Result<(), Fail> {
        restart(worker, a).await?;

        // The config that did not fit now does.
        let short = layout::shortfall(&a.geometry(), worker.config());
        check!(short == 0, "growth left {short} pages unbacked");

        // Both appended runs hand out slots like any other.
        for p in 0..64u64 {
            let addr = at(GREW, p);
            let _ = get_small(worker, a, addr).await;
            put_block(worker, a, addr, &pattern(p as u8 ^ 0x77, SMALL)).await?;
        }
        for p in 0..64u64 {
            let got = get_small(worker, a, at(GREW, p)).await?;
            check!(
                got == pattern(p as u8 ^ 0x77, SMALL),
                "grown page {p} read back wrong"
            );
        }
        let big = big_at(GREW_BIG, 0);
        put_block(worker, a, big, &pattern(0x44, SMALL)).await?;
        let got = get_small(worker, a, big).await?;
        check!(
            got == pattern(0x44, SMALL),
            "grown immutable block read back wrong"
        );
        Ok(())
    }

    async fn corrupted(worker: &server::Worker, a: &'static Allocator) -> Result<(), Fail> {
        // The asymmetry, and it follows mutability rather than width: both blocks are
        // 4 KiB, but only the mutable one carries a data checksum, so only it is refused.
        let r = get_small(worker, a, at(MUTABLE, ROTTED)).await;
        check!(
            r == Err(Status::Missing),
            "a mutable block failing its checksum must never be served, got {}",
            brief(&r)
        );
        // The immutable block is handed back wrong, silently: no checksum by design.
        let got = get_small(worker, a, big_at(BIG, 0)).await?;
        check!(
            got != pattern(0x33, SMALL),
            "the immutable block was supposed to be damaged"
        );

        // Damage is contained to the page: its neighbors are untouched.
        let got = get_small(worker, a, at(MUTABLE, 0)).await?;
        check!(got == pattern(0x5a, SMALL), "neighbor page damaged");
        let got = get_small(worker, a, at(MUTABLE, 2)).await?;
        check!(got == pattern(2, SMALL), "neighbor page damaged");
        Ok(())
    }

    /// Both copies of one metadata block are gone, so its entries are gone and nothing
    /// names the pages they described. `peers` says whether consensus has somewhere to
    /// heal from: with peers a miss on that shard is `Missing`, never served and never a
    /// vote; without them it stays a silent hole, the single-node limitation.
    async fn quarantined(
        worker: &server::Worker,
        a: &'static Allocator,
        peers: bool,
    ) -> Result<(), Fail> {
        let want = if peers { Status::Missing } else { Status::Hole };
        let lost = LOST.lock().unwrap().clone();
        check!(!lost.is_empty(), "the wrecked mblock held no pages");
        for &addr in &lost {
            let r = get_small(worker, a, GlobalAddr(addr)).await;
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
            let addr = at(MUTABLE, p);
            if p == ROTTED || lost.contains(&addr.0) {
                continue;
            }
            let got = get_small(worker, a, addr).await.map_err(|e| {
                Fail(format!(
                    "mutable page {p} ({:#x}) unreadable: {e:?}",
                    addr.0
                ))
            })?;
            check!(
                got == pattern(p as u8, SMALL),
                "mutable page {p} damaged by an unrelated mblock"
            );
            served += 1;
        }
        check!(served > 0, "no page survived");
        let got = get_small(worker, a, big_at(BIG, 1023)).await;
        check!(
            got.is_ok(),
            "the immutable class shares nothing with the mutable one"
        );

        // A page never written is indistinguishable from one whose entry was in the lost
        // block, so it degrades with it. Spread wide enough to hit the affected shard.
        let mut degraded = 0;
        for p in 2000..2064u64 {
            let r = get_small(worker, a, at(MUTABLE, p)).await;
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

    async fn put_block(
        worker: &server::Worker,
        a: &'static Allocator,
        addr: GlobalAddr,
        data: &[u8],
    ) -> Result<u64, Status> {
        let mut pb = PoolBuf::alloc(SMALL).await;
        pb.copy_from_slice(data);
        let g = a.guard(worker, addr).await?;
        a.accept_block(worker, addr, g, Ballot::ZERO, &pb).await
    }

    async fn get_small(
        worker: &server::Worker,
        a: &'static Allocator,
        addr: GlobalAddr,
    ) -> Result<Vec<u8>, Status> {
        let mut pb = PoolBuf::alloc(SMALL).await;
        let v = a.read_block(worker, addr, &mut pb).await;
        // Only the node whose device served a read may record the OCC observation, so the
        // allocator never does: `Paxos::read` records in production, the caller here.
        match v {
            Ok(r) => a.observed(worker, addr, r.version).await,
            Err(Status::Hole) => a.observed(worker, addr, 0).await,
            Err(_) => {}
        }
        v?;
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
        sized(dev, dev_bytes())
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
            extent id=1 base=0 blocks=4096 kind=mutable zone=1
            extent id=2 base=4096 blocks=256 kind=mutable zone=1
            extent id=3 base=5120 blocks=1024 kind=immutable zone=1
            extent id=4 base=6144 blocks=2048 kind=immutable zone=1
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
        grown_at(dev, grown_bytes())
    }

    fn grown_at(dev: &Path, bytes: u64) -> String {
        format!(
            "{}
            extent id=5 base=8192 blocks=8192 kind=mutable zone=1
            extent id=6 base=17408 blocks=2048 kind=immutable zone=1
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
        assert_eq!(std::fs::metadata(&dev).unwrap().len(), dev_bytes());
        layout::format(&dev, &cfg).unwrap();

        run(&dev, 0);
        run(&dev, 1);

        // The same extra capacity with no room in the store: valid config, and the layout
        // refuses, naming the store size. Nothing written, so the next step grows it.
        let cramped = Config::parse(&grown_at(&dev, dev_bytes())).unwrap();
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
        assert_eq!(std::fs::metadata(&dev).unwrap().len(), grown_bytes());
        run_with(&dev, 2, &config_grown(&dev));
        // Asking again for what it already has is a no-op.
        layout::grow_if_needed(&dev, &grown_cfg).unwrap();
        assert_eq!(std::fs::metadata(&dev).unwrap().len(), grown_bytes());

        // Rot one byte of one page in each class, then bring it back up.
        let geo = layout::read_geometry(&dev).unwrap();
        let s = slot_of(&dev, Class::Mutable, at(MUTABLE, ROTTED)).expect("mutable block placed");
        let h = slot_of(&dev, Class::Immutable, big_at(BIG, 0)).expect("immutable block placed");
        flip_byte(&dev, geo.slot_off(Class::Mutable, s) + 17);
        flip_byte(&dev, geo.slot_off(Class::Immutable, h) + 17);
        run(&dev, 3);

        // Now lose a whole metadata block: both copies, so the entries it held are
        // unrecoverable locally and the pages they named have no other record.
        let (id, live) = mblock_of(&dev, Class::Mutable, at(MUTABLE, 0)).expect("mblock");
        *LOST.lock().unwrap() = live
            .into_iter()
            .filter(|a| {
                crate::config::universe_of(*a) == UNIVERSE && crate::config::lba_of(*a) < 4096
            })
            .collect();
        wreck_mblock(&dev, Class::Mutable, id);
        run(&dev, 4);
        run_with(&dev, 5, &config_peered(&dev));

        let _ = std::fs::remove_file(&dev);
    }
}
