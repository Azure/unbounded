//! The allocator: page placement, IO, and the cross-core hops into shard state.
//!
//! Durable formats live in `layout.rs`, the decisions in `shard.rs`; this file is the
//! running state around them. One `Shard` per worker core, no locks, no atomics, no
//! background threads. A page is placed by a free-list pop, written out of place, and
//! its metadata rides a group-committed 4 KiB mblock write.
//!
//! The two classes differ deliberately. `Small` (4 KiB) carries a CRC in its mblock
//! entry, verified on every read, so a page that does not match is never served; `Huge`
//! (4 MiB) carries none, so ordering is its only defence against a torn or lost data
//! write. Both write data before the entry naming it, but only the huge class needs
//! that for the bytes' sake — `finish_small` gives the small class's own reason.
//!
//! Policy lives elsewhere: anti-entropy in `heal.rs`, CASPaxos in `paxos.rs`.

mod shard;

use std::cell::RefCell;
use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll, Waker};
use std::time::Instant;

use crate::config::{Config, Kind, Live};
use crate::heal::{self, Tuple};
use crate::layout::{self, Class, Geometry, MBLOCK};
use crate::paxos::{Ballot, Register};
use crate::runtime::{self, Buf, Disk, Durability, PoolBuf};

use shard::Ticket;
use shard::{Act, Lookup, Shape, Shard, Staged};
pub use shard::{GlobalAddr, Pressure, Status};

/// A reservation whose page is durable but whose entry is not installed yet. Opaque
/// on purpose: the ticket inside never leaves the allocator.
pub struct Pending {
    class: Class,
    ticket: Ticket,
    crc: u32,
}

/// DRAM cost of one resident small page: the mblock entry plus its share of the index.
/// `config::validate` refuses a config whose small working set exceeds
/// `policy.max_index_bytes` rather than letting the node OOM later.
pub const INDEX_BYTES_PER_PAGE: u64 = 52;

/// DRAM cost of one OCC read record: the map entry, its share of the table, and its
/// position in the pool's order.
const OCC_BYTES_PER_RECORD: u64 = 48;

// ------------------------------------------------------------------------- allocator

/// One worker core's state. The decisions live in `Shard` (`shard.rs`); what is left
/// here is the two things that cannot cross into a model — the registered staging
/// buffers and the wakers of parked committers.
struct Core {
    shard: Shard,
    /// Woken by every flush completion on this core. Waiters re-check their own
    /// condition, so a spurious wake costs one poll.
    waiters: Vec<Waker>,
    /// 4 KiB mblock serialisation buffer per class, pre-held by `tick` so the flush
    /// path never has to await one.
    staging: [Option<PoolBuf>; 2],
    /// 4 MiB pages being reassembled from the pieces a transport split them into.
    parts: Vec<Parts>,
}

/// One 4 MiB page arriving in pieces. The slot is reserved by the first piece so the
/// rest land where the page will live, and `have` says which blocks are durable —
/// the class carries no checksum, so a hole must be caught before the entry that
/// names the page is written, never after.
struct Parts {
    addr: GlobalAddr,
    /// The command the pieces belong to. Two commands for one address can be in
    /// flight at once — a member's accept and a non-member's proposal — and their
    /// bytes must never be mixed into a page nothing checksums.
    key: PartsKey,
    ticket: Ticket,
    /// Pieces whose write is still in flight. An assembly is only evicted while this
    /// is zero: giving the slot back under a write in flight would hand it to another
    /// page and let the two overwrite each other.
    busy: u32,
    have: [u64; (layout::HUGE_PAGE / layout::SMALL_PAGE / 64) as usize],
    blocks: u32,
}

/// Guard, ballot and proposer index: what makes two pieces parts of one page.
type PartsKey = (u64, u32, u8);

/// Assemblies one core will hold at once. A transfer that stops halfway leaves its
/// slot reserved until the eighth assembly after it needs the room, which is the
/// whole of the reclamation story: bounded, and no timer to get wrong.
const HUGE_PARTS: usize = 8;

/// Blocks in a 4 MiB page.
const HUGE_BLOCKS: u32 = (layout::HUGE_PAGE / layout::SMALL_PAGE) as u32;

pub struct Allocator {
    disk: Disk,
    geo: Geometry,
    cfg: Live<Config>,
    cores: usize,
    shards: Box<[RefCell<Core>]>,
    /// Consensus side state as the superblock held it at startup: `promised_term` per
    /// group and the seal table. `paxos` takes it from here and owns it thereafter.
    boot: layout::Consensus,
    /// Metadata blocks lost at startup, surfaced as a health metric.
    pub quarantined: usize,
}

// Sound because shard `i` is only ever borrowed from the worker pinned to core `i`,
// and never across an await. `open` leaks the allocator so that hop closures, which
// must be `Send + 'static`, can carry a reference to it.
unsafe impl Sync for Allocator {}

/// Cores that participate in a class's index sharding.
///
/// Mblocks are striped `id % cores` and a core allocates only from its own stripe, so
/// capping the shard width at the mblock count keeps every owning core able to
/// allocate. It matters for the huge class: one mblock covers 504 MiB, so a modest
/// huge slab has fewer mblocks than the machine has workers.
fn shards_for(cores: usize, geo: &Geometry, class: Class) -> usize {
    cores.min(geo.mblocks(class).max(1) as usize)
}

/// This core's share of the OCC pool.
///
/// Partitioned per core because a record is only touched by the core that owns its
/// address, and a shared pool would need a lock on the hot path. The cost is eviction
/// order: a core can drop a record another would have kept, which turns a would-be
/// success into a conflict — a retry, never a wrong answer.
fn occ_per_core(bytes: u64, cores: usize) -> usize {
    (bytes / OCC_BYTES_PER_RECORD / cores.max(1) as u64).max(1) as usize
}

/// The shape the device's geometry implies. The only place production numbers meet
/// the shard code; `shard::model` supplies its own.
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
    fn core_of(&self, core: usize) -> std::cell::RefMut<'_, Core> {
        self.shards[core].borrow_mut()
    }

    /// The decision-making half of this core's state. Every caller here closes the
    /// borrow before its next await, which is the invariant `unsafe impl Sync` rests
    /// on.
    fn shard(&self, core: usize) -> std::cell::RefMut<'_, Shard> {
        std::cell::RefMut::map(self.shards[core].borrow_mut(), |c| &mut c.shard)
    }

    /// The core that owns an address's index shard, and therefore allocates for it.
    /// Reuses the consensus group mapping so the allocator lookup rides the hop the
    /// consensus layer already makes.
    fn owner(&self, addr: GlobalAddr, class: Class) -> usize {
        self.config().group(addr.0) as usize % shards_for(self.cores, &self.geo, class)
    }

    /// The configuration currently in force. Reads are a single load: the control
    /// thread swaps a new one in during a reload and the previous generation stays
    /// alive until the one after that.
    pub fn config(&self) -> &Config {
        self.cfg.get()
    }

    /// Adopt a new configuration. Control thread only, inside the build step of a
    /// reload, once the file has passed every check.
    pub fn install(&self, cfg: Config) {
        self.cfg.install(cfg);
    }

    /// The extent's page kind and class, and its volume's tombstone epoch. All three
    /// come out of one volume lookup because every caller that needs the epoch already
    /// needs the kind. The class is the volume's, not the extent's: page size is uniform
    /// across a volume.
    fn extent(&self, addr: GlobalAddr) -> Option<(Kind, Class, u64)> {
        let v = self.config().volume(addr.volume())?;
        let e = v.extent_at(addr.offset() as u64)?;
        Some((
            e.kind,
            if v.huge { Class::Huge } else { Class::Small },
            v.tombstone_epoch,
        ))
    }

    /// The extent's page kind, and whether the address falls in the huge class.
    pub fn kind_of(&self, addr: GlobalAddr) -> Result<(Kind, bool), Status> {
        let (kind, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        Ok((kind, class == Class::Huge))
    }

    /// The core owning an address, for a caller that wants to ride the same hop. The
    /// cache shards itself by this exact function so its lookups cost nothing extra.
    pub fn owner_core(&self, addr: GlobalAddr) -> Result<usize, Status> {
        let (_, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        Ok(self.owner(addr, class))
    }

    /// The registered device. Shared with the cache, which lives in its own statically
    /// carved region of the same namespace.
    pub fn disk(&self) -> Disk {
        self.disk.clone()
    }

    /// Where the cache region sits, for a caller that owns no `Geometry` of its own.
    pub fn geometry(&self) -> Geometry {
        self.geo
    }

    /// Free-space state of this core's shards. Group hashing spreads addresses
    /// uniformly, so the local view is representative of the device.
    pub fn pressure(&self) -> Pressure {
        self.shard(runtime::core()).pressure()
    }

    /// Whether the device's rate budget is committed far enough ahead that optional work
    /// should stand down. A separate axis from `pressure`, which is about free space:
    /// the device can be nearly empty and still have nothing left to give this second.
    pub fn device_pressed(&self) -> bool {
        self.disk.pressed()
    }

    /// Total time transfers have been held back by the rate budget, in microseconds.
    pub fn device_waited_us(&self) -> u64 {
        self.disk.waited_us()
    }

    /// Free and total page slots in this core's shards, small then huge. Per core
    /// because that is where the free lists live; the exporter sums them.
    pub fn capacity(&self) -> [(u64, u64); 2] {
        self.shard(runtime::core()).capacity()
    }

    /// Live and tombstoned pages per volume in this core's shards,
    /// `(volume, live, tombstones)`. Per core for the same reason as `capacity`; the
    /// exporter sums them.
    pub fn census(&self) -> Vec<(u32, u64, u64)> {
        self.shard(runtime::core()).census()
    }

    pub fn cores(&self) -> usize {
        self.cores
    }

    // ------------------------------------------------------------------ reservations

    /// Guard check plus free-list pop, all on the owning core. Synchronous: no IO has
    /// been issued when this returns, so a refusal costs nothing.
    ///
    /// A present `guard` is the collision detector and the whole of the type check:
    /// LWW, OCC and Immutable differ only in which version the proposer presented.
    /// `None` asks the shard to derive it from the local row instead.
    fn reserve(
        &self,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        guard: Option<u64>,
        ballot: Ballot,
    ) -> Result<Ticket, Status> {
        let epoch = self.config().tombstone_epoch_of(addr.0);
        self.shard(runtime::core())
            .reserve(addr, kind, class, guard, ballot, epoch)
    }

    /// The owning core's half of a commit: install the entry, retire the address's
    /// previous slot, and mark the mblock dirty. Returns what `flush_until` must reach
    /// for this commit to be durable.
    fn stage(
        &self,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        crc: u32,
    ) -> Result<Option<Staged>, Status> {
        let kind = self.extent(addr).ok_or(Status::Unmapped)?.0;
        let cfg = self.config();
        let gof = |a: u64| cfg.group(a);
        self.shard(runtime::core())
            .stage(addr, kind, class, t, crc, &gof)
    }

    /// Undo a reservation whose data write failed, so the slot is not leaked.
    fn unreserve(&self, class: Class, t: Ticket) {
        let cfg = self.config();
        let gof = |a: u64| cfg.group(a);
        self.shard(runtime::core()).unreserve(class, t, &gof);
    }

    // ------------------------------------------------------------------- group commit

    /// Wait until mblock `li` is durable at or past `need`. The first arrival issues
    /// the write immediately with no timer; everyone who commits while it is in flight
    /// rides the next one.
    async fn flush_until(&'static self, class: Class, li: u32, need: u64) -> Result<(), Status> {
        let core = runtime::core();
        loop {
            // Bound the borrow to this statement: it must not survive into the await.
            let act = self.shard(core).flush_act(class, li, need);
            match act {
                Act::Done => return Ok(()),
                Act::Wait => Park::new(self, core).await,
                Act::Go => self.flush(class, li).await?,
            }
        }
    }

    /// Serialise one mblock from its DRAM image and write the copy that is not
    /// current. Always a whole 4 KiB block: there is nothing to read first.
    async fn flush(&'static self, class: Class, li: u32) -> Result<(), Status> {
        let core = runtime::core();
        // Staging is normally pre-held by `tick`; awaiting here is the cold path.
        if self.core_of(core).staging[class as usize].is_none() {
            let b = match PoolBuf::try_alloc(MBLOCK) {
                Some(b) => b,
                None => PoolBuf::alloc(MBLOCK).await,
            };
            self.core_of(core).staging[class as usize] = Some(b);
        }
        let (seq, off, buf) = {
            let mut c = self.core_of(core);
            let c = &mut *c;
            let (seq, h, rows) = c.shard.begin_flush(class, li);
            let stage = c.staging[class as usize].as_mut().unwrap();
            layout::put_mblock(stage, h, rows);
            let off = self
                .geo
                .mblock_off(class, h.mblock_id, (h.generation % 2) as u8);
            (seq, off, stage.buf())
        };
        let r = self.disk.write(off, buf, Durability::Durable).await;
        let mut c = self.core_of(core);
        c.shard.end_flush(class, li, seq, r.is_ok());
        for w in c.waiters.drain(..) {
            w.wake();
        }
        r.map_err(|_| Status::Io)
    }

    // -------------------------------------------------------------------- write paths

    /// The proposer's own leg of an accept, stopped one step short of the register.
    /// The page is durable and the slot is held, but this node still reads as it did
    /// before, so a proposal that never reaches a quorum leaves no trace here.
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
        let t = runtime::on_core(owner, move || async move {
            self.reserve(addr, kind, class, Some(guard), ballot)
        })
        .await?;
        let crc = self.write_page(addr, class, t, page).await?;
        Ok(Pending {
            class,
            ticket: t,
            crc,
        })
    }

    /// Make the page durable behind a ticket we already hold, giving the slot back if
    /// the device write fails. Returns the checksum the entry will carry.
    async fn write_page(
        &'static self,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        page: &PoolBuf,
    ) -> Result<u32, Status> {
        let crc = layout::page_crc(addr.0, t.version, page);
        let off = self.geo.slot_off(class, t.slot);
        if self
            .disk
            .write(off, page.buf(), Durability::Durable)
            .await
            .is_err()
        {
            let owner = self.owner(addr, class);
            runtime::on_core(owner, move || async move { self.unreserve(class, t) }).await;
            return Err(Status::Io);
        }
        Ok(crc)
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
        let t = runtime::on_core(owner, move || async move {
            self.reserve(addr, kind, class, Some(guard), ballot)
        })
        .await?;
        let off = self.geo.slot_off(class, t.slot);
        if self
            .disk
            .write(off, buf, Durability::Durable)
            .await
            .is_err()
        {
            self.abandon(
                addr,
                Pending {
                    class,
                    ticket: t,
                    crc: 0,
                },
            )
            .await;
            return Err(Status::Io);
        }
        Ok(Pending {
            class,
            ticket: t,
            crc: 0,
        })
    }

    /// Install a staged entry, now that a quorum holds the value. A refusal means our
    /// row moved while the peers were answering, so the version we would report is
    /// not ours to give.
    pub async fn finish(&'static self, addr: GlobalAddr, p: Pending) -> Result<u64, Status> {
        let (class, t, crc) = (p.class, p.ticket, p.crc);
        let owner = self.owner(addr, class);
        let done = runtime::on_core(owner, move || async move {
            self.commit(addr, class, t, crc).await
        })
        .await?;
        if done {
            Ok(t.version)
        } else {
            Err(Status::Conflict { current: 0 })
        }
    }

    /// Give the slot back: this proposal never reached a quorum.
    pub async fn abandon(&'static self, addr: GlobalAddr, p: Pending) {
        let (class, t) = (p.class, p.ticket);
        let owner = self.owner(addr, class);
        runtime::on_core(owner, move || async move { self.unreserve(class, t) }).await;
    }

    /// The member side of consensus: apply this page iff the guard still matches. An
    /// error here leaves the register untouched, which is what lets a recovery read a
    /// version off two acceptors and know the bytes behind it exist.
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
        let t = runtime::on_core(owner, move || async move {
            self.reserve(addr, kind, class, guard, ballot)
        })
        .await?;
        self.finish_small(addr, class, t, page).await
    }

    /// Data first, then the entry that names it. Shared by the guarded and unguarded
    /// paths: once a ticket exists they are the same write.
    ///
    /// The ordering is not about torn reads — the checksum catches those — but about
    /// the register: an acceptor that answered no must still read as it did before, or
    /// two acceptors whose data writes failed leave a version behind that looks chosen
    /// to every later recovery and that nobody can ever serve.
    async fn finish_small(
        &'static self,
        addr: GlobalAddr,
        class: Class,
        t: Ticket,
        page: &PoolBuf,
    ) -> Result<Option<u64>, Status> {
        let crc = self.write_page(addr, class, t, page).await?;
        let owner = self.owner(addr, class);
        let done = runtime::on_core(owner, move || async move {
            self.commit(addr, class, t, crc).await
        })
        .await?;
        Ok(done.then_some(t.version))
    }

    /// The 4 MiB member side. With no checksum the only defence against a torn or
    /// lost data write is ordering, so the data must be durable before the entry that
    /// names it is issued.
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
        let t = runtime::on_core(owner, move || async move {
            self.reserve(addr, kind, class, guard, ballot)
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
            runtime::on_core(owner, move || async move { self.unreserve(class, t) }).await;
            return Err(Status::Io);
        }
        let done = runtime::on_core(owner, move || async move {
            self.commit(addr, class, t, 0).await
        })
        .await?;
        Ok(done.then_some(t.version))
    }

    /// One piece of a 4 MiB page a transport split on the way here. Pieces are written
    /// straight into the slot the page will occupy, from whichever core received them,
    /// so nothing copies and nothing has to cross a core.
    ///
    /// Returns the staged page once the last piece is durable, and `None` while any
    /// block is still owed: with no checksum on the class, a page must not reach the
    /// register until every byte of it is on the device.
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
        let t = runtime::on_core(owner, move || async move {
            self.open_parts(addr, kind, class, key, guard, ballot)
        })
        .await?;
        let at = self.geo.slot_off(class, t.slot) + off as u64;
        if self.disk.write(at, buf, Durability::Durable).await.is_err() {
            // The assembly is dropped rather than left short: the initiator retries the
            // whole command, and a slot held for a page nobody will finish is waste.
            runtime::on_core(owner, move || async move { self.drop_parts(addr, key) }).await;
            return Err(Status::Io);
        }
        let first = (off as u64 / blk) as u32;
        let n = (len / blk) as u32;
        let full = runtime::on_core(owner, move || async move {
            self.mark_parts(addr, key, first, n)
        })
        .await;
        Ok(full.map(|ticket| Pending {
            class,
            ticket,
            crc: 0,
        }))
    }

    /// Read a staged page back. The proxied path needs the bytes it assembled in order
    /// to propose them, and the slot is the only place they exist as a whole page.
    pub async fn read_pending(&'static self, p: &Pending, buf: Buf) -> Result<(), Status> {
        let off = self.geo.slot_off(p.class, p.ticket.slot);
        self.disk.read(off, buf).await.map_err(|_| Status::Io)
    }

    /// Find or start an assembly, and count the piece about to be written into it.
    /// Owner core only.
    fn open_parts(
        &self,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        key: PartsKey,
        guard: u64,
        ballot: Ballot,
    ) -> Result<Ticket, Status> {
        let core = runtime::core();
        if let Some(p) = self.find_parts(core, addr, key) {
            let mut c = self.core_of(core);
            let p = &mut c.parts[p];
            p.busy += 1;
            return Ok(p.ticket);
        }
        let t = self.reserve(addr, kind, class, Some(guard), ballot)?;
        let mut c = self.core_of(core);
        if c.parts.len() >= HUGE_PARTS {
            // Idle assemblies first; if every one of them has a write in flight there is
            // no room, and a refusal the initiator retries beats corrupting a page.
            let Some(i) = c.parts.iter().position(|p| p.busy == 0) else {
                drop(c);
                self.unreserve(class, t);
                return Err(Status::Io);
            };
            let old = c.parts.remove(i);
            drop(c);
            self.unreserve(class, old.ticket);
            c = self.core_of(core);
        }
        c.parts.push(Parts {
            addr,
            key,
            ticket: t,
            busy: 1,
            have: [0; (layout::HUGE_PAGE / layout::SMALL_PAGE / 64) as usize],
            blocks: 0,
        });
        Ok(t)
    }

    /// Record blocks now durable, and take the assembly out once the page is whole.
    /// Owner core only.
    fn mark_parts(&self, addr: GlobalAddr, key: PartsKey, first: u32, n: u32) -> Option<Ticket> {
        let core = runtime::core();
        let i = self.find_parts(core, addr, key)?;
        let mut c = self.core_of(core);
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

    /// Drop an assembly and give its slot back. A sibling piece may still be writing
    /// into the slot, so the last piece out is the one that releases it; the blocks the
    /// failed piece owed are never marked, so the page can no longer complete either
    /// way. Owner core only.
    fn drop_parts(&self, addr: GlobalAddr, key: PartsKey) {
        let core = runtime::core();
        let Some(i) = self.find_parts(core, addr, key) else {
            return;
        };
        let mut c = self.core_of(core);
        c.parts[i].busy -= 1;
        if c.parts[i].busy != 0 {
            return;
        }
        let p = c.parts.remove(i);
        drop(c);
        self.unreserve(Class::Huge, p.ticket);
    }

    fn find_parts(&self, core: usize, addr: GlobalAddr, key: PartsKey) -> Option<usize> {
        self.core_of(core)
            .parts
            .iter()
            .position(|p| p.addr == addr && p.key == key)
    }

    // -------------------------------------------------------------- consensus surface

    /// The register as this node holds it, with no data read. A page we have never
    /// seen is not an error: it sits at version zero, or at `3 * epoch` for an
    /// Immutable extent, and consensus needs that to be a vote. Only a page we are
    /// supposed to hold and cannot serve reports `Missing`, which is not a vote.
    pub async fn register(&'static self, addr: GlobalAddr) -> Result<Register, Status> {
        let owner = self.owner_core(addr)?;
        runtime::on_core(owner, move || async move { self.register_local(addr) }).await
    }

    /// [`register`](Self::register) without the hop, for a caller already standing on
    /// the owning core. `paxos` uses it to bump the cache's sketch and read the
    /// register in one hop rather than two.
    pub fn register_local(&self, addr: GlobalAddr) -> Result<Register, Status> {
        let (kind, class, epoch) = self.extent(addr).ok_or(Status::Unmapped)?;
        debug_assert_eq!(runtime::core(), self.owner(addr, class));
        self.shard(runtime::core())
            .register_of(addr, kind, class, epoch)
    }

    /// The guard a proposer on this node should present. The OCC read ring is consulted
    /// here, which is why acceptors need no read-tracking state of their own.
    pub async fn guard(&'static self, addr: GlobalAddr) -> Result<u64, Status> {
        let (kind, class, epoch) = self.extent(addr).ok_or(Status::Unmapped)?;
        runtime::on_core(self.owner(addr, class), move || async move {
            self.shard(runtime::core())
                .guard_of(addr, kind, class, epoch)
        })
        .await
    }

    /// Record a read this node's own ublk device served, which is the only thing that
    /// feeds the OCC ring. Serving a peer is not a read of ours.
    pub async fn observed(&'static self, addr: GlobalAddr, version: u64) {
        let Some((kind, class, _)) = self.extent(addr) else {
            return;
        };
        runtime::on_core(self.owner(addr, class), move || async move {
            self.shard(runtime::core()).observed(addr, kind, version);
        })
        .await
    }

    /// The unguarded apply-if-newer write: the repair step and `learn`, which are the
    /// same operation with different provenance. Legal only underneath a prepare, or
    /// into a migration destination. `Ok(false)` means what we already hold is at least
    /// as new, which makes the two migration streams commutative and a repeated repair
    /// free.
    ///
    /// `replace_equal` also rewrites a row we already hold at exactly `r`, which is how
    /// a repair replaces bytes that failed their checksum under an unchanged register.
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
            let crc = self.write_page(addr, class, t, page).await?;
            let owner = self.owner(addr, class);
            return runtime::on_core(owner, move || {
                Box::pin(self.commit_replace_equal(addr, class, t, crc))
            })
            .await;
        }
        Ok(self.finish_small(addr, class, t, page).await?.is_some())
    }

    /// Learn a register with no bytes behind it: an Immutable tombstone. Nothing can
    /// be pulled for one — no member holds a page to serve — so the register is the
    /// whole of the value, and a replica that never saw the trim would otherwise sit at
    /// the fill point and diverge from its group for good.
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
        runtime::on_core(owner, move || async move {
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
        runtime::on_core(self.owner(addr, class), move || async move {
            self.shard(runtime::core()).reserve_unguarded(
                addr,
                kind,
                class,
                r,
                epoch,
                replace_equal,
            )
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
        let Some(st) = self.stage(addr, class, t, crc)? else {
            return Ok(false);
        };
        self.flush_until(class, st.li, st.seq).await?;
        // Only now is it safe to give the address's previous slot back.
        if let Some(old) = st.stale {
            let cfg = self.config();
            let gof = |a: u64| cfg.group(a);
            self.shard(runtime::core()).release(class, old, &gof);
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
        let Some(st) = self.stage(addr, class, t, crc)? else {
            return Ok(false);
        };
        self.flush_until(class, st.li, st.seq).await?;
        let Some(slot) = st.stale else {
            return Ok(true);
        };
        let holder = (slot / class.k() % self.cores as u32) as usize;
        let retire = move || async move {
            let cfg = self.config();
            let gof = |a: u64| cfg.group(a);
            let flush = self.shard(runtime::core()).release(class, slot, &gof);
            if let Some((li, seq)) = flush {
                self.flush_until(class, li, seq).await?;
            }
            Ok(())
        };
        if holder == runtime::core() {
            retire().await
        } else {
            runtime::on_core(holder, retire).await
        }?;
        Ok(true)
    }

    // --------------------------------------------------------------------- read paths

    /// 4 KiB read. Verified in place against the entry's seeded CRC; a page that does
    /// not match is never served.
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
        let l =
            runtime::on_core(owner, move || async move { self.lookup(addr, kind, class) }).await?;
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

    /// 4 MiB read, straight into the caller's buffer, which may be the guest's own
    /// pages. Not verified: this class has no checksum by design.
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
        let l =
            runtime::on_core(owner, move || async move { self.lookup(addr, kind, class) }).await?;
        let at = self.geo.slot_off(class, l.slot) + off as u64;
        self.disk.read(at, buf).await.map_err(|_| Status::Io)?;
        Ok(Self::reg_of(&l))
    }

    /// Serve a hole by reading the format-time zero region: one device read, and the
    /// CPU never memsets the buffer.
    pub async fn read_zeroes(&'static self, buf: Buf) -> Result<(), Status> {
        self.disk
            .read(self.geo.zero_base, buf)
            .await
            .map_err(|_| Status::Io)
    }

    /// The register the bytes we just read belong to. Reading it apart from them lets
    /// an accept land in between, and a value would then travel under a version it was
    /// never written at.
    fn reg_of(l: &Lookup) -> Register {
        Register {
            version: l.version,
            ballot: Ballot::from_raw(l.ballot as u32),
        }
    }

    fn lookup(&self, addr: GlobalAddr, kind: Kind, class: Class) -> Result<Lookup, Status> {
        self.shard(runtime::core()).lookup(addr, kind, class)
    }

    // ----------------------------------------------------------------------- discard

    /// The member side of a trim proposal. An immutable page becomes a tombstone so a
    /// reader can still tell a hole from a trim, and the entry is reclaimed once the
    /// control plane advances the epoch past it; a mutable page is released outright.
    /// The immutable guard is the page's fill version, `3*epoch + 1`; a repeat is `Ok`
    /// rather than a conflict.
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
        runtime::on_core(owner, move || async move {
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
        let cfg = self.config();
        let epoch = cfg.tombstone_epoch_of(addr.0);
        let gof = |a: u64| cfg.group(a);
        self.shard(runtime::core())
            .trim(addr, kind, class, guard, ballot, epoch, &gof)
    }

    // -------------------------------------------------------- consensus side state

    /// `promised_term` and the seal table as the superblock held them at startup.
    pub fn boot_consensus(&self) -> &layout::Consensus {
        &self.boot
    }

    /// Rewrite the consensus side state through every superblock copy, preserving the
    /// geometry words. Read-modify-write, and deliberately not batched: terms bump on
    /// repair, restart and reconfiguration only, never on the hot path.
    ///
    /// A seal that is acked and then lost is a shard that can be written in two zones,
    /// which is why this gets fourfold redundancy rather than the mblocks' A/B scheme.
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

    /// The core that holds a group's registers, and so its digest and its cursors.
    fn owner_of(&self, group: u32, class: Class) -> usize {
        group as usize % shards_for(self.cores, &self.geo, class)
    }

    /// This node's digest vector for one group and class. Boxed because it crosses a
    /// core boundary and the runtime's reply payload is small.
    pub async fn digests(&'static self, group: u32, huge: bool) -> Box<[u64; heal::BUCKETS]> {
        let class = class_of(huge);
        runtime::on_core(self.owner_of(group, class), move || async move {
            self.shard(runtime::core()).digest_vector(class, group)
        })
        .await
    }

    /// Open an enumeration of a group's registers, narrowed by `filter`.
    pub async fn snap_open(
        &'static self,
        group: u32,
        huge: bool,
        filter: heal::Filter,
    ) -> Result<u32, Status> {
        let class = class_of(huge);
        let core = self.owner_of(group, class);
        runtime::on_core(core, move || async move {
            let now = Instant::now();
            self.shard(runtime::core())
                .snap_start(class, core, huge, group, filter, now)
                .ok_or(Status::NoSpace)
        })
        .await
    }

    /// Next chunk. Bounded by entries scanned as well as tuples produced, so a sparse
    /// filter costs more frames but never a long stall on the owning core.
    pub async fn snap_next(
        &'static self,
        id: u32,
        seq: Option<u8>,
    ) -> Result<(Vec<Tuple>, bool), Status> {
        let (core, huge, _, _) = heal::snap_parts(id);
        if core >= self.cores {
            return Err(Status::Unmapped);
        }
        let class = class_of(huge);
        runtime::on_core(core, move || async move {
            let now = Instant::now();
            let cfg = self.config();
            let gof = |a: u64| cfg.group(a);
            self.shard(runtime::core())
                .snap_next(class, id, seq, &gof, now)
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
        runtime::on_core(core, move || async move {
            self.shard(runtime::core()).snap_stop(class, id);
        })
        .await
    }

    // ------------------------------------------------------------------------- shed

    /// Groups this core still holds registers for, in one class. Per core, like
    /// `census`: the digests live where the registers do, and a group's registers are
    /// all on the one core its id maps to.
    pub fn held_groups(&self, huge: bool) -> Vec<u32> {
        self.shard(runtime::core()).held_groups(class_of(huge))
    }

    /// Forget a group this core has been drained of, so it stops turning up in
    /// `held_groups`.
    pub async fn forget_group(&'static self, group: u32, huge: bool) {
        let class = class_of(huge);
        runtime::on_core(self.owner_of(group, class), move || async move {
            self.shard(runtime::core()).forget_group(class, group);
        })
        .await
    }

    /// Drop a register this node is no longer responsible for. Nothing here checks
    /// that: the caller has confirmed the value is held by every node that now is, and
    /// this only refuses if the register moved since, which makes the confirmation
    /// stale.
    pub async fn discard(&'static self, addr: GlobalAddr, version: u64) -> Result<(), Status> {
        let (_, class, _) = self.extent(addr).ok_or(Status::Unmapped)?;
        let owner = self.owner(addr, class);
        // The mblock index is the owner's, so the flush has to happen there too.
        runtime::on_core(owner, move || async move {
            let cfg = self.config();
            let gof = |a: u64| cfg.group(a);
            // Bound before the match: a temporary in the scrutinee would hold the
            // shard borrow across the flush's await.
            let hit = self
                .shard(runtime::core())
                .discard(addr, class, version, &gof);
            match hit {
                Some((li, need)) => self.flush_until(class, li, need).await,
                None => Ok(()),
            }
        })
        .await
    }

    // -------------------------------------------------------------------- maintenance

    /// Cooperative maintenance, called from the runtime's tick on every worker. Takes
    /// the mblock staging buffers once, then sweeps a bounded slice of this core's
    /// tombstones — the only garbage collection in the system: metadata only, and on
    /// no critical path.
    pub fn tick(&self, now: Instant) {
        let core = runtime::core();
        let cfg = self.config();
        let gof = |a: u64| cfg.group(a);
        // The epoch is the address's volume's, so the sweep resolves it per tombstone
        // rather than being handed one scalar. Bounded by `check_volume`, so the narrow
        // is lossless.
        let eof = |a: u64| cfg.tombstone_epoch_of(a) as u32;
        // Nothing to reclaim until some volume has collected at least once.
        let collecting = cfg.collecting();
        let mut c = self.core_of(core);
        for class in [Class::Small, Class::Huge] {
            if c.staging[class as usize].is_none() {
                c.staging[class as usize] = PoolBuf::try_alloc(MBLOCK);
            }
            c.shard.snap_expire(class, now);
            if collecting {
                c.shard.sweep(class, &eof, &gof);
            }
        }
        c.shard
            .set_occ(occ_per_core(cfg.policy.occ_bytes, self.cores));
        c.shard.set_recoverable(!cfg.node.peers.is_empty());
    }
}

// -------------------------------------------------------------------------- futures

/// Yield once and resume when a flush completes on this core.
struct Park {
    a: &'static Allocator,
    core: usize,
    armed: bool,
}

impl Park {
    fn new(a: &'static Allocator, core: usize) -> Park {
        Park {
            a,
            core,
            armed: false,
        }
    }
}

impl Future for Park {
    type Output = ();

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if self.armed {
            return Poll::Ready(());
        }
        self.armed = true;
        self.a.core_of(self.core).waiters.push(cx.waker().clone());
        Poll::Pending
    }
}

// --------------------------------------------------------------------------- startup

/// Open a formatted device: validate the superblock, rebuild every shard by scanning
/// the metadata region, and hand back an allocator that never needs a journal replay.
///
/// Runs on the control thread, before any worker sees traffic, so the scan can use
/// plain threads and blocking reads. They meter against one budget between them:
/// several threads reading flat out is exactly the case a budget exists for. The
/// returned reference is leaked deliberately: the allocator lives for the process, and
/// cross-core hop closures must be `'static`.
pub fn open(
    path: &std::path::Path,
    disk: Disk,
    cfg: Config,
    cores: usize,
) -> std::io::Result<&'static Allocator> {
    let geo = layout::read_geometry(path)?;
    let boot = layout::read_consensus(path)?;
    let limit = std::sync::Arc::new(runtime::Limiter::new(
        cfg.node.device_max_iops,
        cfg.node.device_max_bytes_per_sec,
    ));
    let scans = scan(path, &geo, cores, &limit)?;

    // Nothing to heal a lost mblock from without peers, so a miss there stays a hole.
    let recoverable = !cfg.node.peers.is_empty();
    // Scoped so the group closure's borrow of `cfg` ends before `cfg` is handed over.
    let (shards, quarantined) = {
        let gof = |a: u64| cfg.group(a);
        shard::rebuild(&shape_of(&geo, &cfg, cores), cores, scans, &gof)
    };
    let shards = shards
        .into_iter()
        .map(|mut shard| {
            shard.set_recoverable(recoverable);
            RefCell::new(Core {
                shard,
                waiters: Vec::with_capacity(64),
                staging: [None, None],
                parts: Vec::new(),
            })
        })
        .collect::<Vec<_>>()
        .into_boxed_slice();

    Ok(Box::leak(Box::new(Allocator {
        disk,
        geo,
        cfg: Live::new(cfg),
        cores,
        shards,
        boot,
        quarantined,
    })))
}

/// Read the whole metadata region and resolve each mblock's A/B copies. Split into
/// contiguous ranges rather than by owning core so every thread reads sequentially;
/// placement into stripes is pure memory work afterwards.
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
        // One batch never crosses an extent boundary: the blocks past it are somewhere
        // else on the device, and both copies are only contiguous within a run.
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

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

/// The allocator on its own, without ublk.
///
/// Pins the guarantees: durability across a restart with no journal, the three type
/// semantics, and the deliberate asymmetry where a corrupted 4 KiB page is refused and
/// a corrupted 4 MiB page is not.
///
/// In-crate rather than in `tests/` so that the allocator's whole surface and the raw
/// disk helpers in `layout` can stay crate-private. It needs root only because the
/// runtime reads the ublk feature set at startup; no device is created.
#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::path::{Path, PathBuf};
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

    const IMG: &str = "racer-alloc.img";
    const DEV_BYTES: u64 = 1 << 30;
    const SMALL: usize = 4096;
    const HUGE: usize = 4 << 20;

    // ---------------------------------------------------------------------------
    // harness
    // ---------------------------------------------------------------------------

    struct Driver;
    static DRIVER: Driver = Driver;

    pub struct Harness {
        alloc: &'static Allocator,
    }

    static PHASE: AtomicUsize = AtomicUsize::new(0);
    static RESULT: Mutex<Option<Result<(), String>>> = Mutex::new(None);
    static STARTED: AtomicUsize = AtomicUsize::new(0);
    /// LWW addresses whose entries were in the mblock the test wrecked.
    static LOST: Mutex<Vec<u64>> = Mutex::new(Vec::new());

    impl Handler for Driver {
        type Config = Harness;

        async fn handle(&'static self, _cfg: Cfg<Harness>, _req: Request) -> Result<(), Errno> {
            Err(Errno::EOPNOTSUPP)
        }

        fn tick(&'static self, cfg: Cfg<Harness>, now: Instant) {
            cfg.alloc.tick(now);
            // Launch the script once, from core 0, onto core 1, so the cross-core path
            // is exercised. `runtime::spawn` is core-local, so the hop is what carries
            // a task elsewhere: polling it once sends the message, and dropping it
            // afterwards abandons only the reply, which we do not want anyway.
            if runtime::core() != 0 || STARTED.swap(1, Ordering::SeqCst) == 1 {
                return;
            }
            let a = cfg.alloc;
            let phase = PHASE.load(Ordering::SeqCst);
            let mut hop = Box::pin(runtime::on_core(1, move || boxed(a, phase)));
            let w = Waker::noop();
            let _ = hop.as_mut().poll(&mut Context::from_waker(w));
        }
    }

    /// Boxed so the hop's payload and task-slot size limits see a fat pointer, not the
    /// whole script.
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
        rt.reload(move |c| {
            let path = PathBuf::from(&cfg.node.device);
            let disk = c.disk(&path, None, None)?;
            let cores = c.cores();
            let alloc = super::open(&path, disk, cfg, cores)?;
            Ok(Harness { alloc })
        })
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

    // ---------------------------------------------------------------------------
    // the script
    // ---------------------------------------------------------------------------

    /// Anything that goes wrong becomes a message the main thread can panic with.
    struct Fail(String);

    impl From<Status> for Fail {
        fn from(s: Status) -> Fail {
            Fail(format!("{s:?}"))
        }
    }

    /// Report a read by length rather than dumping thousands of bytes into a panic
    /// message.
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

    const LWW: u32 = 1;
    const OCC: u32 = 2;
    const IMM: u32 = 3;
    const BIG: u32 = 4;
    /// Only exists once the device has been grown for it.
    const GREW: u32 = 5;
    const GREW_BIG: u32 = 6;

    /// The LWW page whose bytes phase 2 rots. Its checksum never comes back, so every
    /// later phase has to leave it out of what it expects to be served.
    const ROTTED: u32 = 1;

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
        for p in 0..64u32 {
            put_small(a, GlobalAddr::new(LWW, p), &pattern(p as u8, SMALL)).await?;
        }
        for p in 0..64u32 {
            let got = get_small(a, GlobalAddr::new(LWW, p)).await?;
            check!(
                got == pattern(p as u8, SMALL),
                "lww page {p} read back wrong"
            );
        }

        // A page never written is a hole, which is distinct from a page that fails to read.
        let r = get_small(a, GlobalAddr::new(LWW, 1000)).await;
        check!(
            r == Err(Status::Hole),
            "unwritten page should be a hole, got {}",
            brief(&r)
        );

        // Overwriting bumps the version; the page itself moves to a new slot underneath.
        let v0 = put_small(a, GlobalAddr::new(LWW, 0), &pattern(0xa5, SMALL)).await?;
        let v1 = put_small(a, GlobalAddr::new(LWW, 0), &pattern(0x5a, SMALL)).await?;
        check!(v1 > v0, "lww version must advance: {v0} then {v1}");
        let got = get_small(a, GlobalAddr::new(LWW, 0)).await?;
        check!(got == pattern(0x5a, SMALL), "lww overwrite lost");

        // OCC: a write is accepted only against a version this node has read.
        let addr = GlobalAddr::new(OCC, 7);
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
        let addr = GlobalAddr::new(IMM, 3);
        put_small(a, addr, &pattern(9, SMALL)).await?;
        let r = put_small(a, addr, &pattern(10, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "immutable refill must conflict, got {r:?}"
        );
        // A trim leaves a tombstone, which reads as a hole but is not the same thing:
        // the entry survives so a refill still conflicts.
        let dead = GlobalAddr::new(IMM, 4);
        put_small(a, dead, &pattern(11, SMALL)).await?;
        a.accept_trim(
            dead,
            3 * a.config().tombstone_epoch_of(dead.0) + 1,
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
        let addr = GlobalAddr::new(BIG, 0);
        put_huge(a, addr, &pattern(0x33, HUGE)).await?;
        let got = get_huge(a, addr).await?;
        check!(got == pattern(0x33, HUGE), "huge page read back wrong");
        let r = put_huge(a, addr, &pattern(0x44, HUGE)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "huge refill must conflict, got {r:?}"
        );
        let r = get_huge(a, GlobalAddr::new(BIG, 1)).await;
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
        // Before anything here reads and records a fresh observation: the OCC ring is
        // volatile, so every OCC write after a restart conflicts until the page is
        // read again.
        let r = put_small(a, GlobalAddr::new(OCC, 7), &pattern(3, SMALL)).await;
        check!(
            matches!(r, Err(Status::Conflict { .. })),
            "occ pool must come up empty, got {r:?}"
        );

        // No journal, no replay: everything below came back from the metadata scan alone.
        let got = get_small(a, GlobalAddr::new(LWW, 0)).await?;
        check!(
            got == pattern(0x5a, SMALL),
            "lww page 0 lost across restart"
        );
        for p in 1..64u32 {
            let got = get_small(a, GlobalAddr::new(LWW, p)).await?;
            check!(got == pattern(p as u8, SMALL), "lww page {p} lost");
        }
        let got = get_small(a, GlobalAddr::new(OCC, 7)).await?;
        check!(got == pattern(2, SMALL), "occ page lost");
        let got = get_small(a, GlobalAddr::new(IMM, 3)).await?;
        check!(got == pattern(9, SMALL), "immutable page lost");
        let got = get_huge(a, GlobalAddr::new(BIG, 0)).await?;
        check!(got == pattern(0x33, HUGE), "huge page lost");

        // The tombstone survived too, so the distinction between a hole and a trim is
        // durable and not just a property of the running process.
        let dead = GlobalAddr::new(IMM, 4);
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

    /// The device grew under the allocator between the last boot and this one. The whole
    /// point is that nothing already on it moved, so this re-runs the durability checks
    /// unchanged and then uses the slots that were appended.
    async fn grown(a: &'static Allocator) -> Result<(), Fail> {
        restart(a).await?;

        // The config that did not fit now does.
        let short = layout::shortfall(&a.geometry(), a.config());
        check!(short == 0, "growth left {short} pages unbacked");

        // Both appended runs hand out slots like any other, including the huge class,
        // whose data has to stay 4 MiB aligned across the join.
        for p in 0..64u32 {
            put_small(a, GlobalAddr::new(GREW, p), &pattern(p as u8 ^ 0x77, SMALL)).await?;
        }
        for p in 0..64u32 {
            let got = get_small(a, GlobalAddr::new(GREW, p)).await?;
            check!(
                got == pattern(p as u8 ^ 0x77, SMALL),
                "grown page {p} read back wrong"
            );
        }
        let big = GlobalAddr::new(GREW_BIG, 0);
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
        let r = get_small(a, GlobalAddr::new(LWW, ROTTED)).await;
        check!(
            r == Err(Status::Missing),
            "a 4 KiB page failing its checksum must never be served, got {}",
            brief(&r)
        );
        // ...and the 4 MiB page is handed back wrong, silently: this class carries no
        // checksum by design.
        let got = get_huge(a, GlobalAddr::new(BIG, 0)).await?;
        check!(
            got != pattern(0x33, HUGE),
            "the huge page was supposed to be damaged"
        );

        // Damage is contained to the page: its neighbours are untouched.
        let got = get_small(a, GlobalAddr::new(LWW, 0)).await?;
        check!(got == pattern(0x5a, SMALL), "neighbour page damaged");
        let got = get_small(a, GlobalAddr::new(LWW, 2)).await?;
        check!(got == pattern(2, SMALL), "neighbour page damaged");
        Ok(())
    }

    /// Both copies of one metadata block are gone, so the entries it held are gone with
    /// them and nothing on the device names the pages they described.
    ///
    /// `peers` says whether the config gives consensus somewhere to heal from. With
    /// peers a miss on that shard is `Missing` — never served, never a vote. Without
    /// them a miss stays a hole and the loss is silent: the single-node limitation,
    /// asserted here so it stays a decision rather than a surprise.
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
        for p in 0..64u32 {
            let addr = GlobalAddr::new(LWW, p);
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
        let got = get_huge(a, GlobalAddr::new(BIG, 0)).await;
        check!(
            got.is_ok(),
            "the huge class shares nothing with the small one"
        );

        // A page that was never written is indistinguishable from one whose entry was
        // in the block we lost, so on the affected shard it degrades with it. Spread
        // wide enough that at least one address lands there.
        let mut degraded = 0;
        for p in 2000..2064u32 {
            let r = get_small(a, GlobalAddr::new(LWW, p)).await;
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

    // ---------------------------------------------------------------------------
    // page helpers
    // ---------------------------------------------------------------------------

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
        // Only the node whose device served a read may record the OCC observation, so
        // the allocator never records one itself: `Paxos::read` does it in the running
        // system, and here it is the caller's job.
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

    // ---------------------------------------------------------------------------
    // device
    // ---------------------------------------------------------------------------

    /// Eight groups so addresses spread over the workers and the cross-core write path
    /// is exercised rather than short-circuited.
    fn config_text(dev: &Path) -> String {
        format!(
            "
            generation 1
            node id=1 zone=1 device={}
            group 1 2 3
            group 4 5 6
            group 7 8 9
            group 10 11 12
            group 13 14 15
            group 16 17 18
            group 19 20 21
            group 22 23 24
            volume 1 slot=0
              extent pages=4096 kind=lww zone=1
            volume 2 slot=1
              extent pages=256 kind=occ zone=1
            volume 3 slot=2
              extent pages=256 kind=immutable zone=1
            volume 4 slot=3
              extent pages=2 kind=immutable_4m zone=1
            ",
            dev.display()
        )
    }

    /// Where the allocator put a page, read straight out of the metadata region, so the
    /// test can damage exactly one page's bytes behind its back.
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

    /// Two more volumes than the device was formatted for, one per class, so `grow`
    /// has to append a run to each.
    fn config_grown(dev: &Path) -> String {
        format!(
            "{}
            volume 5 slot=4
              extent pages=8192 kind=lww zone=1
            volume 6 slot=5
              extent pages=200 kind=immutable_4m zone=1
            ",
            config_text(dev)
        )
    }

    /// The mblock holding `addr`'s entry, and every live address in it — what the test
    /// is about to make unnameable.
    fn mblock_of(dev: &Path, class: Class, addr: GlobalAddr) -> Option<(u32, Vec<u64>)> {
        let geo = layout::read_geometry(dev).unwrap();
        let f = layout::open_direct(dev, false).unwrap();
        let mut buf = layout::Aligned::new(layout::MBLOCK);
        for id in 0..geo.mblocks(class) as u32 {
            // Take whichever copy is current, as startup does: the stale copy names a
            // different set of pages, and wrecking the block loses the current set
            // whatever the older copy says.
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

    /// Rot both copies, which is the only thing that quarantines: one bad copy is a
    /// lost write and the scan falls back to the other.
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
        // The runtime needs the ublk control node, which requires both root and a
        // kernel carrying `ublk_drv`; probing the node covers both conditions.
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
        {
            let f = std::fs::File::create(&dev).unwrap();
            f.set_len(DEV_BYTES).unwrap();
        }
        let cfg = Config::parse(&config_text(&dev)).unwrap();
        cfg.validate().unwrap();
        layout::format(&dev, &cfg).unwrap();

        run(&dev, 0);
        run(&dev, 1);

        // Add capacity the device was not formatted for, the way `serve` does, then come
        // back up on the larger config.
        let grown_cfg = Config::parse(&config_grown(&dev)).unwrap();
        grown_cfg.validate().unwrap();
        layout::grow_if_needed(&dev, &grown_cfg).unwrap();
        run_with(&dev, 2, &config_grown(&dev));
        // Asking again for what it already has is a no-op.
        layout::grow_if_needed(&dev, &grown_cfg).unwrap();

        // Rot one byte of one page in each class, then bring it back up.
        let geo = layout::read_geometry(&dev).unwrap();
        let s =
            slot_of(&dev, Class::Small, GlobalAddr::new(LWW, ROTTED)).expect("small page placed");
        let h = slot_of(&dev, Class::Huge, GlobalAddr::new(BIG, 0)).expect("huge page placed");
        flip_byte(&dev, geo.slot_off(Class::Small, s) + 17);
        flip_byte(&dev, geo.slot_off(Class::Huge, h) + 17);
        run(&dev, 3);

        // Now lose a whole metadata block: both copies, so the entries it held are
        // unrecoverable locally and the pages they named have no other record.
        let (id, live) = mblock_of(&dev, Class::Small, GlobalAddr::new(LWW, 0)).expect("mblock");
        *LOST.lock().unwrap() = live.into_iter().filter(|a| a >> 32 == LWW as u64).collect();
        wreck_mblock(&dev, Class::Small, id);
        run(&dev, 4);
        run_with(&dev, 5, &config_peered(&dev));

        let _ = std::fs::remove_file(&dev);
    }
}
