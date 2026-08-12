//! The allocator's core state machine.
//!
//! Synchronous: no device, no runtime, no clock. `alloc.rs` owns the IO and cross-core
//! hops, so a refusal here costs nothing. `mod model` below drives these types with
//! `stateright` at an enumerable slot count; the anti-entropy accumulators and snapshot
//! cursors are not modelled and are excluded from shard equality (`mod cmp`, `heal.rs`).

use std::collections::{BTreeMap, HashMap, HashSet, VecDeque};

use crate::config::{self, GroupId, Kind};
use crate::heal::{self, Digests, Groups, Snaps, Tuple};
use crate::layout::{Class, Entry, Header, State};
use crate::paxos::{self, Ballot, Register};
use crate::runtime::Errno;

/// Local mblocks the tombstone sweep examines per `tick`, per class.
const SWEEP_PER_TICK: u32 = 64;

/// Resolves a page address to the extent covering it: its id and its tombstone epoch.
///
/// Passed in like [`Groups`] because the slab holds no config; serves both the census
/// (keyed by extent) and the sweep (needs the epoch). `None` means no extent covers it.
pub(crate) type Extents<'a> = dyn Fn(u64) -> Option<(u32, u32)> + 'a;

/// The two config lookups a slab mutation needs: digest by group, census by extent.
pub(crate) struct Maps<'a> {
    pub(crate) gof: &'a Groups<'a>,
    pub(crate) xof: &'a Extents<'a>,
}

// ---------------------------------------------------------------------------- address

/// A page address: `universe:26 | lba:38`.
///
/// The lba is a block index in the universe's flat 4 KiB address space, the space the
/// control plane places extents in. A 4 MiB page is named by the first of its 1024
/// blocks. Universe id 0 is reserved so a zero address in an mblock entry means "free
/// slot". Nothing about the extent is in the address: an extent may be remapped per
/// node, and the class comes from the config.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Debug, Default)]
pub struct GlobalAddr(pub u64);

impl GlobalAddr {
    pub fn new(universe: u32, lba: u64) -> GlobalAddr {
        GlobalAddr(config::addr_of(universe, lba))
    }

    pub fn universe(self) -> u32 {
        config::universe_of(self.0)
    }

    pub fn lba(self) -> u64 {
        config::lba_of(self.0)
    }
}

/// Why a request could not be served. A hole reads as zeroes, everything else is an
/// error; the block layer must tell them apart.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug)]
pub enum Status {
    /// Never written, or trimmed. Reads as zeroes.
    Hole,
    /// We should hold this page and cannot serve it: failed checksum, or quarantined
    /// metadata. Never served; healed from peers.
    Missing,
    /// Kind semantics refused the write. `current` is the version we hold.
    Conflict {
        current: u64,
    },
    /// The address is not covered by any extent in the config.
    Unmapped,
    /// The slab class is full.
    NoSpace,
    Io,
}

impl Status {
    pub fn errno(self) -> Errno {
        match self {
            Status::Hole => Errno::EIO,
            Status::Missing => Errno::EIO,
            Status::Conflict { .. } => Errno::from_raw(libc::EAGAIN),
            Status::Unmapped => Errno::EINVAL,
            Status::NoSpace => Errno::ENOSPC,
            Status::Io => Errno::EIO,
        }
    }

    /// A peer's error, back to a status. Unrecognised means `Io`: a remote error we
    /// cannot name is not one to act on.
    pub fn from_wire(e: Errno) -> Status {
        match e {
            crate::fabric::status::MISSING => Status::Missing,
            crate::fabric::status::STALE => Status::Conflict { current: 0 },
            crate::fabric::status::NOSPC => Status::NoSpace,
            _ => Status::Io,
        }
    }
}

/// Free-space state of the local shard. `Low` makes `server.rs` delay write
/// completions; `Critical` turns writes into `ENOSPC`.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug)]
pub enum Pressure {
    Normal,
    Low,
    Critical,
}

// ------------------------------------------------------------------------- hash index

/// `GlobalAddr -> global slot id`, open addressed with linear probing. Sized once at
/// startup; an insert past 7/8 full fails rather than growing, since nothing may
/// allocate on the hot path.
#[derive(Clone, PartialEq, Eq, Hash)]
struct HashIndex {
    /// Key and value share a slot: a lookup is one cache line, not two.
    slots: Box<[(u64, u32)]>,
    mask: usize,
    len: usize,
}

impl HashIndex {
    fn new(expect: u64) -> HashIndex {
        let cap = (expect.max(8) * 2).next_power_of_two() as usize;
        HashIndex {
            slots: vec![(0u64, 0u32); cap].into_boxed_slice(),
            mask: cap - 1,
            len: 0,
        }
    }

    fn probe(&self, key: u64) -> usize {
        crate::config::mix(key) as usize & self.mask
    }

    fn find(&self, key: u64) -> Option<usize> {
        let mut i = self.probe(key);
        loop {
            match self.slots[i].0 {
                0 => return None,
                k if k == key => return Some(i),
                _ => i = (i + 1) & self.mask,
            }
        }
    }

    fn get(&self, key: u64) -> Option<u32> {
        self.find(key).map(|i| self.slots[i].1)
    }

    /// Returns the slot this address used to occupy, if any.
    fn insert(&mut self, key: u64, val: u32) -> Option<u32> {
        let mut i = self.probe(key);
        loop {
            match self.slots[i].0 {
                0 => break,
                k if k == key => {
                    let old = self.slots[i].1;
                    self.slots[i].1 = val;
                    return Some(old);
                }
                _ => i = (i + 1) & self.mask,
            }
        }
        if self.len + 1 > self.slots.len() - self.slots.len() / 8 {
            // Sized from the slab's slot count, so this means the group hash is far more
            // skewed than uniform. Refuse rather than probe forever.
            return None;
        }
        self.slots[i] = (key, val);
        self.len += 1;
        None
    }

    fn remove(&mut self, key: u64) -> Option<u32> {
        let mut i = self.find(key)?;
        let val = self.slots[i].1;
        self.len -= 1;
        // Backward-shift deletion: pull up any later key whose ideal position is at or
        // before the hole, so probe chains stay unbroken.
        let mut j = i;
        loop {
            self.slots[i].0 = 0;
            loop {
                j = (j + 1) & self.mask;
                let k = self.slots[j].0;
                if k == 0 {
                    return Some(val);
                }
                let ideal = self.probe(k);
                if (j.wrapping_sub(ideal) & self.mask) >= (j.wrapping_sub(i) & self.mask) {
                    break;
                }
            }
            self.slots[i] = self.slots[j];
            i = j;
        }
    }
}

// ---------------------------------------------------------------------------- occ ring

/// Volatile record of what this node last read for an OCC page. `guard_of` refuses an
/// address with no record, so an empty ring after restart conflicts every in-flight OCC
/// write. Ordered by observation, so a hot page is never evicted ahead of a cold one;
/// superseded positions stay in `fifo` and are skipped at the front, keeping `observe`
/// O(1) at the cost of fewer than `cap` live records for a while.
#[derive(Clone)]
struct OccRing {
    seen: HashMap<u64, (u64, u64)>,
    fifo: VecDeque<(u64, u64)>,
    seq: u64,
    cap: usize,
}

impl OccRing {
    fn new(cap: usize) -> OccRing {
        OccRing {
            seen: HashMap::with_capacity(cap),
            fifo: VecDeque::with_capacity(cap),
            seq: 0,
            cap,
        }
    }

    fn observe(&mut self, addr: u64, version: u64) {
        self.seq += 1;
        self.seen.insert(addr, (version, self.seq));
        self.fifo.push_back((addr, self.seq));
        self.trim();
    }

    /// Drop from the front until the pool is within `cap`. A position whose address was
    /// re-read since is stale and cheap to discard.
    fn trim(&mut self) {
        while self.fifo.len() > self.cap
            && let Some((addr, seq)) = self.fifo.pop_front()
        {
            if self.seen.get(&addr).is_some_and(|&(_, s)| s == seq) {
                self.seen.remove(&addr);
            }
        }
    }

    /// Resize to this core's share of the pool; a config reload can change it.
    fn set_cap(&mut self, cap: usize) {
        self.cap = cap;
        self.trim();
    }

    /// The version this node last read for `addr`, if it is still in the ring.
    fn version(&self, addr: u64) -> Option<u64> {
        self.seen.get(&addr).map(|&(v, _)| v)
    }
}

// --------------------------------------------------------------------------- slab

/// One slab class on one core.
///
/// Slot ids are global within the class. This core owns mblock `m` iff
/// `m % cores == core`, so `local = (m / cores) * k + i` is pure arithmetic and needs no
/// ownership table. `entries` is not a cache of the on-disk mblocks: it *is* their
/// content, so a metadata write is a full-block rewrite with no read-modify-write.
struct Slab {
    core: u32,
    cores: u32,
    pub(super) k: u32,
    /// Number of mblocks in this core's stripe.
    pub(super) local: u32,
    pub(super) entries: Box<[Entry]>,
    pub(super) generation: Box<[u64]>,
    /// Both copies failed their CRC at startup: never allocated, never read.
    pub(super) quarantined: Box<[bool]>,
    /// Any mblock in this stripe was quarantined, so an index miss here could be a page
    /// whose entry was in the lost block. Derived from `quarantined`, cached because
    /// every miss asks.
    lost: bool,
    index: HashIndex,
    /// Entries for indexed pages whose slot is in another core's stripe. Empty unless the
    /// index shard width changed since the last boot.
    foreign: HashMap<u64, Entry>,
    /// Free *local* slot ids, popped from the back. Built descending at startup so pops
    /// ascend and consecutive allocations share an mblock, which is all an "open mblock"
    /// needs to be.
    free: Vec<u32>,
    /// Free slots on loan to the cache. Still free for `pressure` and `capacity`: a loan
    /// is recallable and must not move the watermarks lending is gated on. Never
    /// persisted, since nothing on disk points at a cached page, so a slot on loan at a
    /// crash comes back free.
    lent: HashSet<u32>,
    commit_seq: Box<[u64]>,
    durable_seq: Box<[u64]>,
    flushing: bool,
    sweep: u32,
    /// Anti-entropy accumulators over this slab's registers, keyed by consensus group.
    /// Seeded once by `rebuild`, then maintained incrementally by `set`.
    digests: Digests,
    /// Live and tombstoned entries per extent, sorted by extent id. Maintained by `set`,
    /// seeded by `rebuild`: the control plane cannot advance an extent's epoch without
    /// knowing whether anything in it is live. Bounded by `config::MAX_EXTENTS`.
    census: Vec<(u32, u32, u32)>,
    /// Open enumerations and the reclamation they hold back.
    snaps: Snaps,
}

impl Slab {
    fn new(core: u32, cores: u32, k: u32, mblocks: u64, expect_pages: u64) -> Slab {
        // Ids `core, core+cores, ...` below `mblocks`.
        let local = if (core as u64) < mblocks {
            ((mblocks - core as u64 - 1) / cores as u64 + 1) as u32
        } else {
            0
        };
        let n = local as usize * k as usize;
        Slab {
            core,
            cores,
            k,
            local,
            entries: vec![Entry::default(); n].into_boxed_slice(),
            generation: vec![0u64; local as usize].into_boxed_slice(),
            quarantined: vec![false; local as usize].into_boxed_slice(),
            lost: false,
            index: HashIndex::new(expect_pages),
            foreign: HashMap::new(),
            free: Vec::with_capacity(n),
            lent: HashSet::new(),
            commit_seq: vec![0u64; local as usize].into_boxed_slice(),
            durable_seq: vec![0u64; local as usize].into_boxed_slice(),
            flushing: false,
            sweep: 0,
            digests: Digests::default(),
            census: Vec::new(),
            snaps: Snaps::default(),
        }
    }

    fn local_of(&self, slot: u32) -> Option<u32> {
        let m = slot / self.k;
        if m % self.cores != self.core {
            return None;
        }
        Some((m / self.cores) * self.k + slot % self.k)
    }

    fn global_of(&self, local: u32) -> u32 {
        let li = local / self.k;
        (li * self.cores + self.core) * self.k + local % self.k
    }

    /// The durable record for an indexed address, wherever its slot lives.
    fn entry_of(&self, addr: u64) -> Option<(u32, Entry)> {
        let slot = self.index.get(addr)?;
        match self.local_of(slot) {
            Some(l) => Some((slot, self.entries[l as usize])),
            None => self.foreign.get(&addr).map(|e| (slot, *e)),
        }
    }

    fn dirty(&mut self, li: u32) -> u64 {
        self.commit_seq[li as usize] += 1;
        self.commit_seq[li as usize]
    }

    /// The one place an entry changes after startup. Routing every mutation through here
    /// keeps the digest incremental (XOR is its own inverse) and lets an open cursor see
    /// every register once. `foreign` entries are excluded: they exist only until the
    /// page relocates on its next write, and a repair is such a write, so the divergence
    /// is self-clearing.
    fn set(&mut self, local: u32, e: Entry, m: &Maps) {
        let old = std::mem::replace(&mut self.entries[local as usize], e);
        if old.addr != 0 {
            self.snaps.retain(&old);
            self.digests.toggle((m.gof)(old.addr), &old);
            self.count(old, -1, m);
        }
        if e.addr != 0 {
            self.digests.toggle((m.gof)(e.addr), &e);
            self.count(e, 1, m);
        }
    }

    /// Move an entry into or out of its extent's census row. Rows appear on the extent's
    /// first entry and go when it holds nothing, so a deleted extent leaves no series.
    fn count(&mut self, e: Entry, by: i32, m: &Maps) {
        let (live, tomb) = match e.state {
            State::Live => (by, 0),
            State::Tombstone => (0, by),
            State::Empty => return,
        };
        // A page whose extent has left the config is counted for nothing.
        let Some((x, _)) = (m.xof)(e.addr) else {
            return;
        };
        let i = match self.census.binary_search_by_key(&x, |r| r.0) {
            Ok(i) => i,
            Err(i) => {
                self.census.insert(i, (x, 0, 0));
                i
            }
        };
        let r = &mut self.census[i];
        r.1 = r.1.saturating_add_signed(live);
        r.2 = r.2.saturating_add_signed(tomb);
        if r.1 == 0 && r.2 == 0 {
            self.census.remove(i);
        }
    }

    /// Return a slot for reuse, or park it if a cursor might still need to walk it.
    fn recycle(&mut self, local: u32) {
        self.snaps.park(&mut self.free, local);
    }

    /// Return a slot to the free list. A no-op for a slot outside this core's stripe;
    /// callers in `alloc.rs` hop to the holding core first.
    fn release(&mut self, slot: u32, m: &Maps) {
        let Some(l) = self.local_of(slot) else { return };
        if self.entries[l as usize].addr == 0 {
            return;
        }
        self.set(l, Entry::default(), m);
        self.recycle(l);
        self.dirty(l / self.k);
    }

    fn pressure(&self) -> Pressure {
        let total = self.entries.len();
        if total == 0 {
            return Pressure::Normal;
        }
        let free = self.free.len() + self.lent.len();
        if free * 200 < total {
            Pressure::Critical
        } else if free * 50 < total {
            Pressure::Low
        } else {
            Pressure::Normal
        }
    }

    /// Free and total slots in this core's stripe. Loans count as free: the allocator can
    /// recall any of them within a reservation.
    fn capacity(&self) -> (u64, u64) {
        (
            (self.free.len() + self.lent.len()) as u64,
            self.entries.len() as u64,
        )
    }

    /// Hand one free slot to the cache, only out of a genuinely idle slab: a quarter of
    /// the stripe stays unlent, so [`Slab::reclaim`] never stands between an ordinary
    /// write and a slot. The pressure test measures `free + lent`, so lending cannot walk
    /// past its own gate.
    fn lend(&mut self) -> Option<u32> {
        if self.pressure() != Pressure::Normal
            || (self.free.len() as u64) * super::LEND_CEILING <= self.entries.len() as u64
        {
            return None;
        }
        let l = self.free.pop()?;
        self.lent.insert(l);
        Some(l)
    }

    /// Take a loan back. `false` if the slot was not on loan, so reclaiming a stale offset
    /// is a no-op rather than a double free.
    fn reclaim(&mut self, local: u32) -> bool {
        if !self.lent.remove(&local) {
            return false;
        }
        self.free.push(local);
        true
    }
}

// ---------------------------------------------------------------------------- tickets

/// Reserved slot plus the register the page will carry once committed.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug)]
pub(super) struct Ticket {
    pub(super) slot: u32,
    pub(super) version: u64,
    pub(super) ballot: Ballot,
    /// The row this reservation was issued against, `None` for an absent address. `stage`
    /// refuses if it has moved, which makes the window safe.
    pub(super) prior: Option<(u64, u64)>,
}

/// What a read needs from the owning core.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug)]
pub(super) struct Lookup {
    pub(super) slot: u32,
    pub(super) version: u64,
    pub(super) ballot: u64,
    pub(super) crc: u32,
}

/// What `flush_until` should do next. Split out of the wait loop so the loop's only shard
/// borrow is synchronous and closed before its await.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(super) enum Act {
    /// Already durable at or past the sequence asked for.
    Done,
    /// Somebody else's write is in flight; park and re-check.
    Wait,
    /// This caller took the flush.
    Go,
}

/// The version a page effectively holds, not always the version in its entry. An
/// `Immutable` page with no entry, or one left by an older epoch, is empty at the
/// *current* epoch and sits at `3 * epoch`, so an epoch advance takes no consensus round:
/// every replica computes the same number from the entry and epoch.
fn effective(e: Option<Entry>, kind: Kind, epoch: u64) -> u64 {
    match (e, kind) {
        (Some(e), Kind::Immutable) if e.version / 3 < epoch => 3 * epoch,
        (Some(e), _) => e.version,
        (None, Kind::Immutable) => 3 * epoch,
        (None, _) => 0,
    }
}

/// The state a freshly accepted version implies. Ordinal 2 of `Immutable`'s
/// `3*epoch + ordinal` encoding is a tombstone, so a trim needs no opcode here: it is an
/// accept one past the fill point.
fn state_of(kind: Kind, version: u64) -> State {
    match kind {
        Kind::Immutable if version % 3 == 2 => State::Tombstone,
        _ => State::Live,
    }
}

// ----------------------------------------------------------------------------- shard

/// What a commit still owes: the mblock to make durable, and the slot the address used
/// to occupy. The stale slot is released only once the new one is durable; freeing it
/// eagerly would let a crash between the two mblock writes leave the address in neither,
/// losing an acknowledged write. Both slots live at a crash is fine: startup resolves the
/// duplicate by `(version, ballot)`.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug)]
pub(super) struct Staged {
    pub li: u32,
    pub seq: u64,
    pub stale: Option<u32>,
}

/// The shape a shard is built to. Production fills it from the device geometry, the
/// model with a handful of slots so the state space can be enumerated.
#[derive(Clone, Copy, Debug)]
pub(super) struct Shape {
    pub cores: u32,
    /// Slots per mblock, per class. `layout::Class::k()` in production.
    pub k: [u32; 2],
    pub mblocks: [u64; 2],
    /// Index capacity hint, per class.
    pub expect: [u64; 2],
    /// Cores that participate in each class's index sharding.
    pub shards_for: [usize; 2],
    /// This core's share of the node-wide OCC pool, in records.
    pub occ: usize,
    /// Re-check the ticket against the entry at commit time. Always `true` in
    /// production; the model flips it to show the check is load-bearing.
    pub recheck: bool,
}

/// One worker core's slice of the allocator: both slab classes plus the OCC ring they
/// share. No buffers and no wakers (those live in `alloc.rs`), so it stays comparable.
pub(super) struct Shard {
    slabs: [Slab; 2],
    recheck: bool,
    /// A page we cannot serve can be healed from a peer. False for a single-node
    /// deployment, where reporting a loss buys nothing and costs every hole.
    recoverable: bool,
    occ: OccRing,
}

impl Shard {
    fn new(core: u32, shape: &Shape) -> Shard {
        Shard {
            slabs: [
                Slab::new(
                    core,
                    shape.cores,
                    shape.k[0],
                    shape.mblocks[0],
                    shape.expect[0],
                ),
                Slab::new(
                    core,
                    shape.cores,
                    shape.k[1],
                    shape.mblocks[1],
                    shape.expect[1],
                ),
            ],
            occ: OccRing::new(shape.occ),
            recheck: shape.recheck,
            recoverable: false,
        }
    }

    /// Take a new share of the OCC pool; a config reload can resize it.
    pub(super) fn set_occ(&mut self, cap: usize) {
        self.occ.set_cap(cap);
    }

    /// Whether this node has peers to heal a lost page from; a config reload can add the
    /// first, so it is not fixed at startup.
    pub(super) fn set_recoverable(&mut self, yes: bool) {
        self.recoverable = yes;
    }

    /// What an index miss means in this class. A shard that lost an mblock cannot tell a
    /// never-written page from one whose entry was in the lost block, so while there is
    /// somewhere to heal from both answer `Missing`: never served, never a vote.
    fn miss(&self, class: Class) -> Status {
        if self.recoverable && self.slabs[class as usize].lost {
            Status::Missing
        } else {
            Status::Hole
        }
    }

    fn slab(&mut self, class: Class) -> &mut Slab {
        &mut self.slabs[class as usize]
    }

    /// Worse of the two classes: either one full is enough to slow the node.
    pub(super) fn pressure(&self) -> Pressure {
        match (self.slabs[0].pressure(), self.slabs[1].pressure()) {
            (Pressure::Critical, _) | (_, Pressure::Critical) => Pressure::Critical,
            (Pressure::Low, _) | (_, Pressure::Low) => Pressure::Low,
            _ => Pressure::Normal,
        }
    }

    /// Free and total slots per class, small then huge. Summed across cores this is the
    /// whole device, because the stripes partition it.
    pub(super) fn capacity(&self) -> [(u64, u64); 2] {
        [self.slabs[0].capacity(), self.slabs[1].capacity()]
    }

    /// Lend the cache one free 4 MiB slot, as a local slot id. Small pages are never
    /// lent: 4 MiB of the small slab is 1024 slots, so a reclaim would drop 1024 cached
    /// pages at once and the DRAM to index them costs more than the media is worth.
    pub(super) fn lend(&mut self) -> Option<u32> {
        let l = self.slab(Class::Huge).lend()?;
        Some(self.slab(Class::Huge).global_of(l))
    }

    /// Take back a lent 4 MiB slot, named by its global slot id.
    pub(super) fn reclaim(&mut self, slot: u32) -> bool {
        let Some(l) = self.slab(Class::Huge).local_of(slot) else {
            return false;
        };
        self.slab(Class::Huge).reclaim(l)
    }

    /// Slots on loan in this core's 4 MiB stripe, which `capacity` is counting as free.
    pub(super) fn lent(&self) -> u64 {
        self.slabs[Class::Huge as usize].lent.len() as u64
    }

    /// Live and tombstoned entries per extent, `(extent, live, tombstones)` sorted by
    /// extent. Summed across cores this is the whole node, which the control plane needs
    /// before advancing an extent's tombstone epoch.
    pub(super) fn census(&self) -> Vec<(u32, u64, u64)> {
        let mut out: Vec<(u32, u64, u64)> = Vec::new();
        for sl in &self.slabs {
            for &(x, live, tomb) in &sl.census {
                match out.binary_search_by_key(&x, |r| r.0) {
                    Ok(i) => {
                        out[i].1 += live as u64;
                        out[i].2 += tomb as u64;
                    }
                    Err(i) => out.insert(i, (x, live as u64, tomb as u64)),
                }
            }
        }
        out
    }

    // ------------------------------------------------------------------ reservations

    /// Guard check plus free-list pop. Synchronous, so a refusal costs no IO. `guard`
    /// absent derives it from the type's own rule; every production caller supplies one.
    /// The guard is the collision detector and the whole type check: LWW, OCC and
    /// Immutable differ only in which version the proposer presented.
    pub(super) fn reserve(
        &mut self,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        guard: Option<u64>,
        ballot: Ballot,
        epoch: u64,
    ) -> Result<Ticket, Status> {
        let old = self.slab(class).entry_of(addr.0).map(|(_, e)| e);
        let current = effective(old, kind, epoch);
        let g = match guard {
            Some(g) => g,
            // The type table's rules: the version we hold for LWW, the last read version
            // for OCC, the epoch's fill point for Immutable.
            None => match kind {
                Kind::Lww => current,
                Kind::Occ => self
                    .occ
                    .version(addr.0)
                    .ok_or(Status::Conflict { current })?,
                Kind::Immutable => 3 * epoch,
            },
        };
        // The protocol's own rule, shared with the model checker that proves it.
        let held = old.map(|e| Ballot::from_raw(e.ballot as u32));
        if !paxos::admits(current, held, g, ballot) {
            return Err(Status::Conflict { current });
        }
        let sl = self.slab(class);
        if sl.pressure() == Pressure::Critical {
            return Err(Status::NoSpace);
        }
        let local = sl.free.pop().ok_or(Status::NoSpace)?;
        let next = Register::accepted(g, ballot);
        Ok(Ticket {
            slot: sl.global_of(local),
            version: next.version,
            ballot: next.ballot,
            prior: old.map(|e| (e.version, e.ballot)),
        })
    }

    /// The unguarded apply-if-newer reservation behind repair and `learn`. `None` means
    /// what we hold is at least as new, making the two migration streams commutative and a
    /// repeated repair free. `replace_equal` also admits an exactly equal live register,
    /// so repair can reinstall bytes that failed their checksum.
    pub(super) fn reserve_unguarded(
        &mut self,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        r: Register,
        epoch: u64,
        replace_equal: bool,
    ) -> Result<Option<Ticket>, Status> {
        let e = self.slab(class).entry_of(addr.0).map(|(_, x)| x);
        let held = (effective(e, kind, epoch), e.map_or(0, |x| x.ballot as u32));
        let equal_live = e.is_some_and(|x| {
            x.state == State::Live && x.version == r.version && x.ballot as u32 == r.ballot.raw()
        });
        // The protocol's own rule, shared with the model checker that proves it.
        if !paxos::supersedes(held, r, replace_equal && equal_live) {
            return Ok(None);
        }
        let sl = self.slab(class);
        if sl.pressure() == Pressure::Critical {
            return Err(Status::NoSpace);
        }
        let local = sl.free.pop().ok_or(Status::NoSpace)?;
        Ok(Some(Ticket {
            slot: sl.global_of(local),
            version: r.version,
            ballot: r.ballot,
            prior: e.map(|x| (x.version, x.ballot)),
        }))
    }

    /// Install the entry, retire the address's previous slot, mark the mblock dirty.
    /// Returns the mblock and the sequence a flush must reach to make this commit
    /// durable.
    pub(super) fn stage(
        &mut self,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        t: Ticket,
        crc: u32,
        m: &Maps,
    ) -> Result<Option<Staged>, Status> {
        let state = state_of(kind, t.version);
        let recheck = self.recheck;
        let sl = self.slab(class);
        let local = sl.local_of(t.slot).ok_or(Status::Io)?;
        // A reservation and its commit are separated by the data write, so another
        // accept, learn or trim can land in between. The guard was checked against the
        // row `reserve` saw, so it holds only if that row has not moved; a trim in the
        // window would otherwise let this stale accept resurrect a deleted value at a
        // version someone else has bound. Comparing the raw row rather than `effective`
        // keeps epoch out of it: an epoch advance touches no row, and an Immutable accept
        // from the old epoch reads as superseded anyway. Giving the slot back and
        // reporting success is right: something newer won.
        let seen = sl.entry_of(addr.0).map(|(_, e)| (e.version, e.ballot));
        if recheck && seen != t.prior {
            sl.recycle(local);
            return Ok(None);
        }
        let e = Entry {
            addr: addr.0,
            version: t.version,
            ballot: t.ballot.raw() as u64,
            data_crc: crc,
            epoch: if state == State::Tombstone {
                (t.version / 3) as u32
            } else {
                0
            },
            state,
            flags: 0,
        };
        sl.set(local, e, m);
        let stale = match sl.index.insert(addr.0, t.slot) {
            Some(old) if old != t.slot => Some(old),
            _ => None,
        };
        sl.foreign.remove(&addr.0);
        let li = local / sl.k;
        Ok(Some(Staged {
            li,
            seq: sl.dirty(li),
            stale,
        }))
    }

    /// Retire the slot a commit displaced, once its replacement is durable. Returns the
    /// mblock and sequence to flush to, or `None` if the slot is outside this stripe.
    pub(super) fn release(&mut self, class: Class, slot: u32, m: &Maps) -> Option<(u32, u64)> {
        let sl = self.slab(class);
        let local = sl.local_of(slot)?;
        sl.release(slot, m);
        Some((local / sl.k, sl.commit_seq[(local / sl.k) as usize]))
    }

    /// Undo a reservation whose data write failed, so the slot is not leaked.
    pub(super) fn unreserve(&mut self, class: Class, t: Ticket, m: &Maps) {
        let sl = self.slab(class);
        if let Some(l) = sl.local_of(t.slot) {
            sl.set(l, Entry::default(), m);
            sl.recycle(l);
        }
    }

    // -------------------------------------------------------------- consensus surface

    /// The register as this node holds it, with no data read. An unseen page is not an
    /// error: it sits at version zero, or `3 * epoch` for an Immutable extent, and
    /// consensus needs that to be a vote.
    pub(super) fn register_of(
        &mut self,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        epoch: u64,
    ) -> Result<Register, Status> {
        let miss = self.miss(class);
        let e = self.slab(class).entry_of(addr.0).map(|(_, e)| e);
        if e.is_none() && miss == Status::Missing {
            return Err(miss);
        }
        Ok(Register {
            version: effective(e, kind, epoch),
            ballot: Ballot::from_raw(e.map_or(0, |e| e.ballot as u32)),
        })
    }

    /// Record what a read this node's *own* ublk device served returned. Answering a peer
    /// deliberately does not land here: an acceptor keeps no read-tracking state, which
    /// makes the OCC check cluster-wide.
    pub(super) fn observed(&mut self, addr: GlobalAddr, kind: Kind, version: u64) {
        if kind == Kind::Occ {
            self.occ.observe(addr.0, version);
        }
    }

    /// The guard a proposer on this node should present.
    pub(super) fn guard_of(
        &mut self,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        epoch: u64,
    ) -> Result<u64, Status> {
        let e = self.slab(class).entry_of(addr.0).map(|(_, e)| e);
        let current = effective(e, kind, epoch);
        match kind {
            Kind::Lww => Ok(current),
            // The version this node read, whatever the local slab holds; a non-member
            // holds nothing. The acceptor's comparison of it against the register *is*
            // the OCC check, so a write with no prior read here cannot be checked and
            // conflicts.
            Kind::Occ => self.occ.version(addr.0).ok_or(Status::Conflict { current }),
            Kind::Immutable => Ok(3 * epoch),
        }
    }

    /// The OCC observation is *not* recorded here: this path also serves a peer's `GET`,
    /// and only the node whose ublk device answered the client may record a read, which
    /// `Paxos::read` does.
    pub(super) fn lookup(
        &mut self,
        addr: GlobalAddr,
        _kind: Kind,
        class: Class,
    ) -> Result<Lookup, Status> {
        let miss = self.miss(class);
        let (slot, e) = self
            .slab(class)
            .entry_of(addr.0)
            .filter(|(_, e)| e.state == State::Live)
            .ok_or(miss)?;
        Ok(Lookup {
            slot,
            version: e.version,
            ballot: e.ballot,
            crc: e.data_crc,
        })
    }

    // ----------------------------------------------------------------------- discard

    /// The member side of a trim proposal. An immutable page becomes a tombstone so
    /// readers can tell a hole from a trim, and is reclaimed when the control plane
    /// advances the epoch past it; a mutable page is released. The immutable case guards
    /// on `3*epoch + 1` and is idempotent, so a repeat is `Ok`.
    #[allow(clippy::too_many_arguments)] // type, class and epoch all ride in
    pub(super) fn trim(
        &mut self,
        addr: GlobalAddr,
        kind: Kind,
        class: Class,
        guard: Option<u64>,
        ballot: Ballot,
        epoch: u64,
        m: &Maps,
    ) -> Result<Option<(u32, u64)>, Status> {
        let sl = self.slab(class);
        let Some((slot, e)) = sl.entry_of(addr.0) else {
            return Ok(None);
        };
        let Some(local) = sl.local_of(slot) else {
            // The slot belongs to another core's stripe; leave it to relocate.
            return Ok(None);
        };
        if kind == Kind::Immutable {
            if e.state != State::Live {
                return Ok(None);
            }
            let current = effective(Some(e), kind, epoch);
            if guard.is_some_and(|g| g != current) {
                return Err(Status::Conflict { current });
            }
            let t = Entry {
                addr: addr.0,
                version: current + 1,
                ballot: ballot.raw() as u64,
                data_crc: 0,
                epoch: ((current + 1) / 3) as u32,
                state: State::Tombstone,
                flags: 0,
            };
            sl.set(local, t, m);
        } else {
            sl.index.remove(addr.0);
            sl.set(local, Entry::default(), m);
            sl.recycle(local);
        }
        let li = local / sl.k;
        Ok(Some((li, sl.dirty(li))))
    }

    // ------------------------------------------------------------------- group commit

    pub(super) fn flush_act(&mut self, class: Class, li: u32, need: u64) -> Act {
        let sl = self.slab(class);
        if sl.durable_seq[li as usize] >= need {
            return Act::Done;
        }
        if sl.flushing {
            Act::Wait
        } else {
            sl.flushing = true;
            Act::Go
        }
    }

    /// Snapshot one mblock: the sequence this write makes durable, the header to stamp on
    /// it, and the rows. The generation is bumped here, before the write is issued, so
    /// the copy this lands in is `generation % 2`.
    pub(super) fn begin_flush(&mut self, class: Class, li: u32) -> (u64, Header, &[Entry]) {
        let sl = &mut self.slabs[class as usize];
        let seq = sl.commit_seq[li as usize];
        let g = sl.generation[li as usize] + 1;
        sl.generation[li as usize] = g;
        let k = sl.k as usize;
        let id = li * sl.cores + sl.core;
        let rows = &sl.entries[li as usize * k..(li as usize + 1) * k];
        let live = rows.iter().filter(|e| e.addr != 0).count() as u16;
        (
            seq,
            Header {
                mblock_id: id,
                generation: g,
                class,
                live,
            },
            rows,
        )
    }

    /// Retire the flush. `durable_seq` only moves forward, and only on success: that is
    /// the predicate every committer waits on.
    pub(super) fn end_flush(&mut self, class: Class, li: u32, seq: u64, ok: bool) {
        let sl = self.slab(class);
        sl.flushing = false;
        if ok {
            sl.durable_seq[li as usize] = sl.durable_seq[li as usize].max(seq);
        }
    }

    // -------------------------------------------------------------------- maintenance

    /// One tick's worth of tombstone reclamation on one class. The only garbage collection
    /// in the system: metadata only, bounded, off the critical path. The extent is
    /// resolved per tombstone because the epoch is the extent's (one extent may collect
    /// while others stand still), and only for entries already known to be tombstones.
    pub(super) fn sweep(&mut self, class: Class, m: &Maps) {
        let sl = self.slab(class);
        if sl.local == 0 {
            return;
        }
        for _ in 0..SWEEP_PER_TICK {
            let li = sl.sweep % sl.local;
            sl.sweep = sl.sweep.wrapping_add(1);
            let k = sl.k as usize;
            let mut hit = false;
            for j in 0..k {
                let l = li as usize * k + j;
                let e = sl.entries[l];
                if e.state == State::Tombstone
                    && let Some((_, epoch)) = (m.xof)(e.addr)
                    && e.epoch < epoch
                {
                    sl.set(l as u32, Entry::default(), m);
                    sl.index.remove(e.addr);
                    sl.recycle(l as u32);
                    hit = true;
                }
            }
            if hit {
                // Persisted by whichever commit next lands on this mblock; a tombstone
                // outliving its epoch costs only an entry.
                sl.dirty(li);
            }
        }
    }

    // ------------------------------------------------------------------- anti-entropy

    pub(super) fn digest_vector(
        &mut self,
        class: Class,
        group: GroupId,
    ) -> Box<[u64; heal::BUCKETS]> {
        self.slab(class).digests.vector(group)
    }

    /// Groups this shard holds registers for. With `forget_group`, how the shed finds
    /// work without scanning.
    pub(super) fn held_groups(&mut self, class: Class) -> Vec<GroupId> {
        self.slab(class).digests.held()
    }

    pub(super) fn forget_group(&mut self, class: Class, group: GroupId) {
        self.slab(class).digests.forget(group);
    }

    /// Forget a register outright, leaving no tombstone: the page is not deleted but left
    /// to the members that now own it, and a tombstone would be a deletion for them to
    /// replay. Conditional on the version the caller confirmed those members hold, so a
    /// write that raced the confirmation is kept.
    pub(super) fn discard(
        &mut self,
        addr: GlobalAddr,
        class: Class,
        version: u64,
        m: &Maps,
    ) -> Option<(u32, u64)> {
        let sl = self.slab(class);
        let (slot, e) = sl.entry_of(addr.0)?;
        // A slot in another core's stripe is that core's to drop.
        let local = sl.local_of(slot)?;
        if e.version != version {
            return None;
        }
        sl.index.remove(addr.0);
        sl.set(local, Entry::default(), m);
        sl.recycle(local);
        let li = local / sl.k;
        Some((li, sl.dirty(li)))
    }

    pub(super) fn snap_start(
        &mut self,
        class: Class,
        core: usize,
        huge: bool,
        group: GroupId,
        filter: heal::Filter,
        now: std::time::Instant,
    ) -> Option<u32> {
        self.slab(class).snaps.start(core, huge, group, filter, now)
    }

    pub(super) fn snap_next(
        &mut self,
        class: Class,
        id: u32,
        seq: Option<u8>,
        universe: Option<u32>,
        m: &Maps,
        now: std::time::Instant,
    ) -> Result<(Vec<Tuple>, bool), Status> {
        let sl = self.slab(class);
        // Split the borrow: the walk reads `entries` while the cursor advances.
        let (snaps, entries) = (&mut sl.snaps, &sl.entries);
        snaps.next(id, seq, universe, entries, m.gof, now)
    }

    pub(super) fn snap_stop(&mut self, class: Class, id: u32) {
        let sl = self.slab(class);
        let mut free = std::mem::take(&mut sl.free);
        sl.snaps.stop(id, &mut free);
        sl.free = free;
    }

    /// A cursor whose reader vanished must not hold reclamation for ever.
    pub(super) fn snap_expire(&mut self, class: Class, now: std::time::Instant) {
        let sl = self.slab(class);
        let mut free = std::mem::take(&mut sl.free);
        sl.snaps.expire(now, &mut free);
        sl.free = free;
    }
}

// --------------------------------------------------------------------------- startup

/// One mblock as read from the device, after the A/B copies have been resolved.
pub(super) struct Scanned {
    pub id: u32,
    pub class: Class,
    pub generation: u64,
    pub quarantined: bool,
    pub entries: Vec<Entry>,
}

/// Which of an mblock's two copies to believe. Highest generation with a valid CRC wins;
/// falling back to the older copy is a lost write, not corruption. Neither valid is the
/// only case that quarantines. The flag names the copy.
pub(super) fn pick_ab(ha: Option<Header>, hb: Option<Header>) -> Option<(Header, bool)> {
    match (ha, hb) {
        (Some(x), Some(y)) if y.generation > x.generation => Some((y, true)),
        (Some(x), _) => Some((x, false)),
        (None, Some(y)) => Some((y, true)),
        (None, None) => None,
    }
}

/// Rebuild every shard from a scan of the metadata region. There is no journal to replay,
/// so whatever the scan found *is* the state; the only work is deciding who owns what.
/// Returns the shards and the count of quarantined mblocks.
pub(super) fn rebuild(
    shape: &Shape,
    cores: usize,
    scans: Vec<Scanned>,
    m: &Maps,
) -> (Vec<Shard>, usize) {
    let mut shards: Vec<Shard> = (0..cores).map(|c| Shard::new(c as u32, shape)).collect();

    // Place every scanned mblock in its owning core's stripe.
    let mut quarantined = 0usize;
    let mut found: Vec<(u64, Class, u32, (u64, u32))> = Vec::new();
    for m in &scans {
        let owner = (m.id % cores as u32) as usize;
        let sl = shards[owner].slab(m.class);
        let li = (m.id / cores as u32) as usize;
        let k = sl.k as usize;
        sl.generation[li] = m.generation;
        if m.quarantined {
            sl.quarantined[li] = true;
            sl.lost = true;
            quarantined += 1;
            continue;
        }
        sl.entries[li * k..(li + 1) * k].copy_from_slice(&m.entries);
        for (j, e) in m.entries.iter().enumerate() {
            if e.addr != 0 && e.state != State::Empty {
                found.push((
                    e.addr,
                    m.class,
                    m.id * sl.k + j as u32,
                    (e.version, e.ballot as u32),
                ));
            }
        }
    }
    drop(scans);

    // Two slots can claim one address after a crash between the metadata write and the
    // old slot's release. The higher version was the acked one; the ballot breaks ties,
    // since two accepts can share a version and any other tiebreak would let a restart
    // resurrect the ballot that lost while the node was up. A `BTreeMap` so the loop below
    // fills the index and the foreign map in key order: a rebuild must land on the same
    // layout every time it sees the same device.
    let mut winner: BTreeMap<u64, (Class, u32, (u64, u32))> = BTreeMap::new();
    let mut losers: Vec<(Class, u32)> = Vec::new();
    for (addr, class, slot, version) in found {
        match winner.get(&addr) {
            Some(&(_, _, pv)) if pv >= version => losers.push((class, slot)),
            Some(&(pc, ps, _)) => {
                losers.push((pc, ps));
                winner.insert(addr, (class, slot, version));
            }
            None => {
                winner.insert(addr, (class, slot, version));
            }
        }
    }
    for (class, slot) in losers {
        let owner = (slot / shape.k[class as usize] % cores as u32) as usize;
        let sl = shards[owner].slab(class);
        if let Some(l) = sl.local_of(slot) {
            sl.entries[l as usize] = Entry::default();
            sl.dirty(l / sl.k);
        }
    }

    // The index shards by consensus group, which need not be the core owning the slot;
    // when they differ the entry goes into the `foreign` side map and the page relocates
    // on its next write.
    for (addr, (class, slot, _)) in &winner {
        let owner = (m.gof)(*addr).index() as usize % shape.shards_for[*class as usize];
        let holder = (*slot / shape.k[*class as usize] % cores as u32) as usize;
        // Read the holder before touching the owner: they may be the same shard.
        let e = if holder == owner {
            None
        } else {
            let hl = shards[holder].slab(*class);
            hl.local_of(*slot).map(|l| hl.entries[l as usize])
        };
        let sl = shards[owner].slab(*class);
        if sl.index.insert(*addr, *slot).is_none()
            && sl.local_of(*slot).is_none()
            && let Some(e) = e
        {
            sl.foreign.insert(*addr, e);
        }
    }
    drop(winner);

    // Free lists are never persisted: whatever is unclaimed after the scan is free.
    // Pushed descending so pops ascend and stay inside one mblock.
    for sh in &mut shards {
        for class in [Class::Small, Class::Huge] {
            let sl = sh.slab(class);
            for l in (0..sl.entries.len()).rev() {
                if sl.entries[l].addr == 0 && !sl.quarantined[l / sl.k as usize] {
                    sl.free.push(l as u32);
                }
            }
            // Seed the anti-entropy accumulators and the per-extent census from what
            // survived the scan: the only full pass either gets, since every later change
            // goes through `set`.
            for l in 0..sl.entries.len() {
                let e = sl.entries[l];
                if e.addr != 0 {
                    sl.digests.toggle((m.gof)(e.addr), &e);
                    sl.count(e, 1, m);
                }
            }
        }
    }

    (shards, quarantined)
}

// ------------------------------------------------------- comparability, for the model

/// `stateright` has to clone, compare and hash whatever it explores, and the types above
/// are ordinary owned data apart from two fields.
///
/// `digests` and `snaps` are excluded: the first is 4 KiB per consensus group, the second
/// holds wall-clock deadlines. Both belong to `heal.rs` and are checked there. The impls
/// are `cfg(test)` so no production path picks up a `Clone` that silently drops them.
#[cfg(test)]
mod cmp {
    use super::*;
    use std::hash::{Hash, Hasher};

    /// The OCC ring as a state. Its sequence numbers count up for ever and only their
    /// ordering and equality are read, so renumbering by rank loses nothing and keeps
    /// the state space finite.
    type Ring = (Vec<(u64, u64)>, Vec<(u64, u64, u64)>);

    fn ring(r: &OccRing) -> Ring {
        let mut seqs: Vec<u64> = r.fifo.iter().map(|&(_, s)| s).collect();
        seqs.sort_unstable();
        let rank = |s: u64| seqs.binary_search(&s).unwrap_or(usize::MAX) as u64;
        let fifo = r.fifo.iter().map(|&(a, s)| (a, rank(s))).collect();
        let mut seen: Vec<(u64, u64, u64)> =
            r.seen.iter().map(|(&a, &(v, s))| (a, v, rank(s))).collect();
        seen.sort_unstable();
        (fifo, seen)
    }

    fn entries(m: &HashMap<u64, Entry>) -> Vec<(u64, Entry)> {
        let mut v: Vec<(u64, Entry)> = m.iter().map(|(k, e)| (*k, *e)).collect();
        v.sort_unstable_by_key(|(k, _)| *k);
        v
    }

    impl PartialEq for OccRing {
        fn eq(&self, o: &OccRing) -> bool {
            self.cap == o.cap && ring(self) == ring(o)
        }
    }

    impl Hash for OccRing {
        fn hash<H: Hasher>(&self, h: &mut H) {
            self.cap.hash(h);
            ring(self).hash(h);
        }
    }

    impl std::fmt::Debug for OccRing {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "occ{:?}", ring(self).1)
        }
    }

    impl Slab {
        /// The sweep cursor counts up for ever but is only read modulo the stripe length,
        /// so that is what a state is. Without this the state space is infinite for no
        /// behavioural reason.
        fn cursor(&self) -> u32 {
            self.sweep % self.local.max(1)
        }
    }

    impl Clone for Slab {
        fn clone(&self) -> Slab {
            Slab {
                core: self.core,
                cores: self.cores,
                k: self.k,
                local: self.local,
                entries: self.entries.clone(),
                generation: self.generation.clone(),
                quarantined: self.quarantined.clone(),
                lost: self.lost,
                index: self.index.clone(),
                foreign: self.foreign.clone(),
                free: self.free.clone(),
                lent: self.lent.clone(),
                commit_seq: self.commit_seq.clone(),
                durable_seq: self.durable_seq.clone(),
                flushing: self.flushing,
                sweep: self.sweep,
                census: self.census.clone(),
                digests: Digests::default(),
                snaps: Snaps::default(),
            }
        }
    }

    impl PartialEq for Slab {
        fn eq(&self, o: &Slab) -> bool {
            self.core == o.core
                && self.k == o.k
                && self.entries == o.entries
                && self.generation == o.generation
                && self.quarantined == o.quarantined
                && self.index == o.index
                && self.free == o.free
                && self.lent == o.lent
                && self.commit_seq == o.commit_seq
                && self.durable_seq == o.durable_seq
                && self.flushing == o.flushing
                && self.cursor() == o.cursor()
                && self.census == o.census
                && entries(&self.foreign) == entries(&o.foreign)
        }
    }

    impl Hash for Slab {
        fn hash<H: Hasher>(&self, h: &mut H) {
            self.core.hash(h);
            self.entries.hash(h);
            self.generation.hash(h);
            self.quarantined.hash(h);
            self.index.hash(h);
            self.free.hash(h);
            self.commit_seq.hash(h);
            self.durable_seq.hash(h);
            self.flushing.hash(h);
            self.cursor().hash(h);
            self.census.hash(h);
            entries(&self.foreign).hash(h);
        }
    }

    impl std::fmt::Debug for Slab {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            let live: Vec<(usize, u64, u64, u32)> = self
                .entries
                .iter()
                .enumerate()
                .filter(|(_, e)| e.addr != 0)
                .map(|(l, e)| (l, e.addr & 0xffff, e.version, e.data_crc))
                .collect();
            let idx: Vec<(u64, u32)> = self
                .index
                .slots
                .iter()
                .filter(|(k, _)| *k != 0)
                .map(|(k, v)| (k & 0xffff, *v))
                .collect();
            write!(
                f,
                "slab{{live:{live:?} idx:{idx:?} free:{:?} gen:{:?} commit:{:?} durable:{:?}}}",
                self.free, self.generation, self.commit_seq, self.durable_seq
            )
        }
    }

    impl Clone for Shard {
        fn clone(&self) -> Shard {
            Shard {
                slabs: [self.slabs[0].clone(), self.slabs[1].clone()],
                occ: self.occ.clone(),
                recheck: self.recheck,
                recoverable: self.recoverable,
            }
        }
    }

    impl PartialEq for Shard {
        fn eq(&self, o: &Shard) -> bool {
            self.slabs == o.slabs && self.occ == o.occ
        }
    }

    impl Hash for Shard {
        fn hash<H: Hasher>(&self, h: &mut H) {
            self.slabs.hash(h);
            self.occ.hash(h);
        }
    }

    impl std::fmt::Debug for Shard {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "{:?} {:?}", self.slabs[0], self.occ)
        }
    }
}

// ------------------------------------------------------------------------ the model

/// Model checks over the code above, with `stateright`.
///
/// These drive the real `Shard`, the same `reserve`, `stage`, `trim`, `sweep`,
/// `flush_act`/`begin_flush`/`end_flush` and `rebuild` that production calls, at a shape
/// small enough to enumerate: two slots per mblock, two mblocks, two addresses.
///
/// The rule under test is a parameter wherever the design had a choice, so each check
/// runs twice: once with the rule the code uses, which must hold, and once with the
/// weaker rule, which must fail.
///
/// No IO, no runtime, no device: these run without root.
#[cfg(test)]
mod model {
    use super::*;
    use stateright::{Checker, HasDiscoveries, Model, Property};
    use std::collections::BTreeSet;

    const K: u32 = 2;
    const MBLOCKS: u64 = 2;
    const SLOTS: usize = (K as u64 * MBLOCKS) as usize;
    const MAX_WRITES: u8 = 3;
    const MAX_PENDING: usize = 2;

    /// One address in each of two extents, in different consensus groups so a change of
    /// core count can separate an index shard from a slot's owner. Separate extents
    /// because the tombstone epoch is per extent. Both sit in universe 1.
    const ADDRS: [u64; 2] = [1 << config::LBA_BITS, (1 << config::LBA_BITS) | 1];
    const GUARDS: [Option<u64>; 4] = [None, Some(0), Some(1), Some(3)];
    /// Trims are guarded too, but only the fill points (`3*epoch + 1`) are interesting.
    const TGUARDS: [Option<u64>; 3] = [None, Some(1), Some(4)];
    const BALLOTS: [(u32, u8); 2] = [(1, 1), (2, 0)];
    const MAX_FLUSHES: u8 = 4;

    /// One mblock's two on-disk copies, each `(generation, rows)`. `None` is a copy that
    /// will not decode, which a torn or failed write leaves behind.
    type Copies = [Option<(u64, Vec<Entry>)>; 2];

    /// A commit that has staged but is not yet durable: address, version, ballot, value,
    /// mblock, the commit sequence it needs, and the slot it displaced.
    type Waiting = (u64, u64, u32, u32, u32, u64, Option<u32>);

    fn group_of(a: u64) -> GroupId {
        GroupId::new(1, (a & 1) as u32)
    }

    /// The two extents hold one address each, so the low bit of an address indexes both
    /// the epochs and the extent ids.
    fn ext_of(addr: u64) -> usize {
        (addr & 1) as usize
    }

    /// Extent ids are one-based, so a census row is never keyed by zero.
    fn ext_id(addr: u64) -> u32 {
        ext_of(addr) as u32 + 1
    }

    /// Bind the lookups a slab mutation needs and bundle them as `$m`, like
    /// `alloc::maps!` does for the real allocator. A macro because the bundle borrows the
    /// closures, so nothing here can be returned.
    macro_rules! maps {
        ($epochs:expr, $m:ident) => {
            let epochs = $epochs;
            let gof = group_of;
            let xof = move |a: u64| Some((ext_id(a), epochs[ext_of(a)] as u32));
            let $m = Maps {
                gof: &gof,
                xof: &xof,
            };
        };
        ($m:ident) => {
            maps!([0u64; ADDRS.len()], $m)
        };
    }

    /// The census a slab should be holding, recomputed the slow way.
    fn recount(sl: &Slab) -> Vec<(u32, u32, u32)> {
        let mut out: Vec<(u32, u32, u32)> = Vec::new();
        for e in sl.entries.iter() {
            let (live, tomb) = match e.state {
                State::Live => (1, 0),
                State::Tombstone => (0, 1),
                State::Empty => continue,
            };
            let v = ext_id(e.addr);
            match out.binary_search_by_key(&v, |r| r.0) {
                Ok(i) => {
                    out[i].1 += live;
                    out[i].2 += tomb;
                }
                Err(i) => out.insert(i, (v, live, tomb)),
            }
        }
        out
    }

    fn shape(cores: u32, recheck: bool) -> Shape {
        Shape {
            cores,
            recheck,
            k: [K, K],
            mblocks: [MBLOCKS, 0],
            expect: [8, 8],
            shards_for: [cores as usize, 1],
            // One record: eviction is unreachable at the production size, and the claim
            // that eviction only turns a success into a conflict is what wants checking.
            occ: 1,
        }
    }

    fn empty_rows() -> Vec<Entry> {
        vec![Entry::default(); K as usize]
    }

    /// Every shard is built the way `open` builds one: free lists are not persisted and
    /// only `rebuild` knows how to derive them.
    fn boot(cores: u32, recheck: bool, image: &[Copies]) -> (Vec<Shard>, usize) {
        let scans = (0..MBLOCKS as usize)
            .map(|id| {
                let hdr = |c: usize| {
                    image[id][c].as_ref().map(|(g, _)| Header {
                        mblock_id: id as u32,
                        generation: *g,
                        class: Class::Small,
                        live: 0,
                    })
                };
                match pick_ab(hdr(0), hdr(1)) {
                    Some((h, b)) => Scanned {
                        id: id as u32,
                        class: Class::Small,
                        generation: h.generation,
                        quarantined: false,
                        entries: image[id][b as usize].as_ref().unwrap().1.clone(),
                    },
                    None => Scanned {
                        id: id as u32,
                        class: Class::Small,
                        generation: 0,
                        quarantined: true,
                        entries: Vec::new(),
                    },
                }
            })
            .collect();
        maps!(m);
        rebuild(&shape(cores, recheck), cores as usize, scans, &m)
    }

    fn formatted() -> Vec<Copies> {
        (0..MBLOCKS)
            .map(|_| [Some((0, empty_rows())), Some((0, empty_rows()))])
            .collect()
    }

    /// Where a leak or a dangling read would show up: a slot is free or occupied, never
    /// both and never twice, and every index key names a slot that holds that address.
    fn sound(shards: &[Shard]) -> bool {
        for sh in shards {
            let sl = &sh.slabs[0];
            let mut seen = vec![false; sl.entries.len()];
            for &l in sl.free.iter() {
                let l = l as usize;
                if l >= seen.len() || seen[l] || sl.entries[l].addr != 0 {
                    return false;
                }
                seen[l] = true;
            }
            for &(k, v) in sl.index.slots.iter() {
                // A key whose slot sits in another core's stripe is a foreign entry, which
                // this slab holds no row for.
                if k != 0
                    && let Some(l) = sl.local_of(v)
                    && sl.entries[l as usize].addr != k
                {
                    return false;
                }
            }
        }
        true
    }

    /// What a reader sees. Reads go through the index, never by scanning slots, so with a
    /// displaced slot retired late (`Staged::stale`) two slots may hold one address but
    /// only one is reachable.
    fn find(shards: &[Shard], addr: u64) -> Option<Entry> {
        shards
            .iter()
            .find_map(|sh| sh.slabs[0].entry_of(addr).map(|(_, e)| e))
    }

    /// Every slot beyond the one the index names must be a stale slot some commit still
    /// holds. Anything else is a leak.
    fn duplicates_are_owned(shards: &[Shard], held: &BTreeSet<u32>) -> bool {
        // The index naming a row need not live on the core holding it: the index shards
        // by consensus group and the slots by core.
        let mut named: BTreeSet<u32> = BTreeSet::new();
        for sh in shards {
            let sl = &sh.slabs[0];
            for &(k, v) in sl.index.slots.iter() {
                if k != 0 {
                    named.insert(v);
                }
            }
        }
        shards.iter().all(|sh| {
            let sl = &sh.slabs[0];
            sl.entries.iter().enumerate().all(|(l, e)| {
                let g = sl.global_of(l as u32);
                e.addr == 0 || named.contains(&g) || held.contains(&g)
            })
        })
    }

    // -------------------------------------------------------------- register semantics

    /// One core, no IO. Everything a page's type semantics decide happens here, so this
    /// is where the type table and the guard rule are pinned.
    #[derive(Clone, PartialEq, Hash, Debug)]
    struct Reg {
        sh: Shard,
        /// Reserved, not yet staged. A ticket in flight is a slot nobody else can have.
        pending: Vec<(u64, Ticket, bool)>,
        /// (address, version, ballot, value) this node has accepted.
        acked: BTreeSet<(u64, u64, u32, u32)>,
        /// Every version this node ever observed for an address. The OCC ring may forget
        /// from this set but never invent outside it.
        seen: BTreeSet<(u64, u64)>,
        /// Guards the ring actually let through on the derived path.
        used: BTreeSet<(u64, u64)>,
        /// One per extent, the scope the control plane advances.
        epoch: [u64; ADDRS.len()],
        /// Set for an extent whose tombstone the sweep has actually reclaimed.
        reaped: [bool; ADDRS.len()],
        writes: u8,
        /// Reservations dropped without `unreserve`: the slot is not returned until the
        /// next boot.
        leaked: u8,
        /// Successful mutable trims, so the model can prove the path is reached.
        trimmed: u8,
        /// Set if an Immutable accept presenting the epoch's fill point came back at a
        /// version that is not `3*epoch + 1`. Learns are excluded: a repair stream carries
        /// whatever version it was given, and apply-if-newer is its only rule.
        offbeat: bool,
    }

    #[derive(Clone, Copy, PartialEq, Debug)]
    enum RegAct {
        Lookup(u8),
        Register(u8),
        Guard(u8),
        Learn { a: u8, version: u64, b: u8 },
        Reserve { a: u8, g: u8, b: u8 },
        Stage(u8),
        Leak(u8),
        Unreserve(u8),
        Trim { a: u8, g: u8 },
        AdvanceEpoch(u8),
        Sweep,
    }

    struct RegModel {
        kind: Kind,
        /// `Commit::Checked` is what the allocator does. `Commit::Blind` installs whatever
        /// the ticket says without re-reading the entry the reservation was granted
        /// against.
        commit: Commit,
    }

    #[derive(Clone, Copy, PartialEq, Debug)]
    enum Commit {
        Checked,
        Blind,
    }

    impl Model for RegModel {
        type State = Reg;
        type Action = RegAct;

        fn init_states(&self) -> Vec<Reg> {
            vec![Reg {
                sh: boot(1, self.commit == Commit::Checked, &formatted())
                    .0
                    .pop()
                    .unwrap(),
                pending: Vec::new(),
                acked: BTreeSet::new(),
                seen: BTreeSet::new(),
                used: BTreeSet::new(),
                epoch: [0; ADDRS.len()],
                reaped: [false; ADDRS.len()],
                writes: 0,
                leaked: 0,
                trimmed: 0,
                offbeat: false,
            }]
        }

        fn actions(&self, s: &Reg, out: &mut Vec<RegAct>) {
            for a in 0..ADDRS.len() as u8 {
                out.push(RegAct::Lookup(a));
                out.push(RegAct::Register(a));
                out.push(RegAct::Guard(a));
                for g in 0..TGUARDS.len() as u8 {
                    out.push(RegAct::Trim { a, g });
                }
                if s.writes < MAX_WRITES && s.pending.len() < MAX_PENDING {
                    for g in 0..GUARDS.len() as u8 {
                        for b in 0..BALLOTS.len() as u8 {
                            out.push(RegAct::Reserve { a, g, b });
                        }
                    }
                    for version in 1..=2u64 {
                        out.push(RegAct::Learn { a, version, b: 1 });
                    }
                }
            }
            for p in 0..s.pending.len() as u8 {
                out.push(RegAct::Stage(p));
                out.push(RegAct::Leak(p));
                out.push(RegAct::Unreserve(p));
            }
            for v in 0..ADDRS.len() as u8 {
                // Each extent's epoch moves on its own. One step is enough: nothing in the
                // dataplane branches on the size of the jump, only on the ordering.
                if s.epoch[v as usize] == 0 {
                    out.push(RegAct::AdvanceEpoch(v));
                }
            }
            out.push(RegAct::Sweep);
        }

        fn next_state(&self, last: &Reg, act: RegAct) -> Option<Reg> {
            let mut s = last.clone();
            let kind = self.kind;
            let cls = Class::Small;
            match act {
                RegAct::Lookup(a) => {
                    let ad = GlobalAddr(ADDRS[a as usize]);
                    let v = s.sh.lookup(ad, kind, cls).map_or(0, |l| l.version);
                    s.seen.insert((ad.0, v));
                }
                RegAct::Register(a) => {
                    let ad = GlobalAddr(ADDRS[a as usize]);
                    let r =
                        s.sh.register_of(ad, kind, cls, s.epoch[a as usize])
                            .unwrap();
                    s.seen.insert((ad.0, r.version));
                }
                RegAct::Guard(a) => {
                    let ad = GlobalAddr(ADDRS[a as usize]);
                    let _ = s.sh.guard_of(ad, kind, cls, s.epoch[a as usize]);
                }
                RegAct::Learn { a, version, b } => {
                    let ad = GlobalAddr(ADDRS[a as usize]);
                    let ballot = Ballot::new(BALLOTS[b as usize].0, BALLOTS[b as usize].1);
                    let r = Register { version, ballot };
                    if let Ok(Some(t)) =
                        s.sh.reserve_unguarded(ad, kind, cls, r, s.epoch[a as usize], false)
                    {
                        s.pending.push((ad.0, t, false));
                        s.writes += 1;
                    }
                }
                RegAct::Reserve { a, g, b } => {
                    let ad = GlobalAddr(ADDRS[a as usize]);
                    let guard = GUARDS[g as usize];
                    let ballot = Ballot::new(BALLOTS[b as usize].0, BALLOTS[b as usize].1);
                    if let Ok(t) =
                        s.sh.reserve(ad, kind, cls, guard, ballot, s.epoch[a as usize])
                    {
                        // Only the guards a real proposer can present: `guard_of` returns
                        // the epoch's fill point for Immutable, so an accept either
                        // derives it or carries that number. An arbitrary guard is covered
                        // by the conflict rules instead.
                        let fill = guard.is_none_or(|g| g == 3 * s.epoch[a as usize]);
                        s.offbeat |= kind == Kind::Immutable && fill && t.version % 3 != 1;
                        // A derived guard is the one the OCC ring vouched for.
                        if guard.is_none() && kind == Kind::Occ {
                            s.used.insert((ad.0, t.version - 1));
                        }
                        s.pending.push((ad.0, t, guard.is_none()));
                        s.writes += 1;
                    }
                }
                RegAct::Stage(p) => {
                    let (addr, t, _) = s.pending.remove(p as usize);
                    // The value rides in the entry's data checksum, so two proposers at
                    // one version are distinguishable in the durable record.
                    let value = t.ballot.raw();
                    // `Ok(None)` is a commit that lost the race and gave its slot back:
                    // the caller sees success but nothing was written, so nothing is
                    // acknowledged.
                    maps!(m);
                    if let Ok(Some(st)) = s.sh.stage(GlobalAddr(addr), kind, cls, t, value, &m) {
                        s.acked.insert((addr, t.version, t.ballot.raw(), value));
                        if let Some(old) = st.stale {
                            s.sh.release(cls, old, &m);
                        }
                    }
                }
                RegAct::Leak(p) => {
                    s.pending.remove(p as usize);
                    s.leaked += 1;
                }
                RegAct::Unreserve(p) => {
                    let (_, t, _) = s.pending.remove(p as usize);
                    maps!(m);
                    s.sh.unreserve(cls, t, &m);
                }
                RegAct::Trim { a, g } => {
                    let ad = GlobalAddr(ADDRS[a as usize]);
                    let ballot = Ballot::new(2, 0);
                    maps!(m);
                    let r = s.sh.trim(
                        ad,
                        kind,
                        cls,
                        TGUARDS[g as usize],
                        ballot,
                        s.epoch[a as usize],
                        &m,
                    );
                    // A mutable trim destroys the register rather than tombstoning it, so
                    // versions restart from zero and the history before it is no longer a
                    // claim about the same incarnation. Forgetting it scopes the register
                    // property to "between trims".
                    if kind != Kind::Immutable && matches!(r, Ok(Some(_))) {
                        s.acked.retain(|&(x, _, _, _)| x != ad.0);
                        s.trimmed += 1;
                    }
                }
                RegAct::AdvanceEpoch(v) => s.epoch[v as usize] += 1,
                RegAct::Sweep => {
                    // Which tombstones the sweep took, and whose: an extent the control
                    // plane has not advanced loses nothing.
                    let before: Vec<u64> = s.sh.slabs[0]
                        .entries
                        .iter()
                        .filter(|e| e.state == State::Tombstone)
                        .map(|e| e.addr)
                        .collect();
                    maps!(s.epoch, m);
                    s.sh.sweep(cls, &m);
                    for a in before {
                        if find(std::slice::from_ref(&s.sh), a).is_none() {
                            s.reaped[ext_of(a)] = true;
                        }
                    }
                }
            }
            Some(s)
        }

        fn properties(&self) -> Vec<Property<Self>> {
            let mut ps = vec![
                // A ballot names one value at one version, once and for all. Two values at
                // one *version* is legal: two proposers may propose at one version with
                // different ballots and only one is chosen, so the key is the pair.
                Property::<Self>::always("one value per ballot", |_, s| {
                    let mut v: Vec<(u64, u64, u32)> =
                        s.acked.iter().map(|&(a, x, b, _)| (a, x, b)).collect();
                    v.sort_unstable();
                    v.dedup();
                    v.len() == s.acked.len()
                }),
                // The register never goes backwards. Compared through `effective`, because
                // an epoch advance legitimately carries an Immutable address forward past
                // a tombstone the sweep has since reclaimed.
                Property::<Self>::always("the register never regresses", |m: &Self, s: &Reg| {
                    s.acked.iter().all(|&(a, v, b, _)| {
                        let e = find(std::slice::from_ref(&s.sh), a);
                        let held = (
                            effective(e, m.kind, s.epoch[ext_of(a)]),
                            e.map_or(0, |x| x.ballot as u32),
                        );
                        held >= (v, b)
                    })
                }),
                // No version is ever accepted at zero: `reserve` returns `g + 1`.
                Property::<Self>::always("accepted versions are nonzero", |_, s| {
                    s.acked.iter().all(|&(_, v, _, _)| v > 0)
                }),
                // Every slot is free, live, reserved or leaked, exactly once. Catches a
                // double free, a lost slot, and a stale index entry.
                Property::<Self>::always("slots are accounted for", |_, s| {
                    if !sound(std::slice::from_ref(&s.sh)) {
                        return false;
                    }
                    let sl = &s.sh.slabs[0];
                    let live = sl.entries.iter().filter(|e| e.addr != 0).count();
                    // The index names the slot the entry actually occupies, both ways.
                    for (l, e) in sl.entries.iter().enumerate() {
                        if e.addr == 0 {
                            continue;
                        }
                        match sl.index.get(e.addr).and_then(|g| sl.local_of(g)) {
                            Some(x) if x as usize == l => {}
                            _ => return false,
                        }
                    }
                    sl.free.len() + live + s.pending.len() + s.leaked as usize == SLOTS
                }),
                // An accept on the derived path is only ever licensed by a version this
                // node really read. Eviction may drop a record, turning a success into a
                // conflict; it may never manufacture one.
                Property::<Self>::always("occ accepts were observed", |_, s| {
                    s.used.is_subset(&s.seen)
                }),
                // The census is maintained one entry at a time in `set`; this is the only
                // check that it still agrees with the slab, and the control plane's epoch
                // decision rests on it.
                Property::<Self>::always("the census matches the slab", |_, s| {
                    s.sh.slabs[0].census == recount(&s.sh.slabs[0])
                }),
                Property::<Self>::sometimes("a value is accepted", |_, s| !s.acked.is_empty()),
                Property::<Self>::sometimes("a slot is reused", |_, s| {
                    s.writes as usize > SLOTS - s.sh.slabs[0].free.len() + s.leaked as usize
                }),
            ];
            match self.kind {
                Kind::Immutable => {
                    // `3*epoch + ordinal`: a fill is ordinal 1, a trim is ordinal 2.
                    // Nothing else may be accepted, at any epoch.
                    ps.push(Property::<Self>::always(
                        "immutable versions are ordinals",
                        |_, s| !s.offbeat,
                    ));
                    ps.push(Property::<Self>::sometimes(
                        "a tombstone is written",
                        |_, s| {
                            s.sh.slabs[0]
                                .entries
                                .iter()
                                .any(|e| e.state == State::Tombstone)
                        },
                    ));
                    // The sweep is scoped to one extent: an extent whose epoch still sits
                    // at zero keeps every tombstone, however far ahead the other runs.
                    ps.push(Property::<Self>::always(
                        "the sweep stays inside its extent",
                        |_, s| (0..ADDRS.len()).all(|v| s.epoch[v] > 0 || !s.reaped[v]),
                    ));
                    ps.push(Property::<Self>::sometimes(
                        "a tombstone is reclaimed",
                        |_, s| {
                            s.acked.iter().any(|&(a, v, _, _)| {
                                v == 2
                                    && s.epoch[ext_of(a)] > 0
                                    && find(std::slice::from_ref(&s.sh), a).is_none()
                            })
                        },
                    ));
                    // One extent collects while the other, still lagging, keeps its own
                    // tombstone.
                    ps.push(Property::<Self>::sometimes(
                        "one extent collects alone",
                        |_, s| {
                            s.reaped[0]
                                && s.epoch[1] == 0
                                && s.sh.slabs[0]
                                    .entries
                                    .iter()
                                    .any(|e| e.state == State::Tombstone && ext_of(e.addr) == 1)
                        },
                    ));
                }
                _ => {
                    // A mutable trim does not tombstone: it drops the entry and hands the
                    // slot straight back.
                    ps.push(Property::<Self>::sometimes(
                        "a trim frees a slot",
                        |_, s| {
                            s.trimmed > 0
                                && find(std::slice::from_ref(&s.sh), ADDRS[0]).is_none()
                                && !s.sh.slabs[0].free.is_empty()
                        },
                    ));
                }
            }
            ps
        }
    }

    // ------------------------------------------------------------------- durability

    /// Where a flush lands. Alternating is the A/B ping-pong; fixed is the alternative in
    /// which a failed write destroys the only copy there was.
    #[derive(Clone, Copy, PartialEq, Debug)]
    enum Ab {
        Alternating,
        Fixed,
    }

    /// When a write may be reported as done. Durable is what `flush_until` waits for;
    /// staged is what the DRAM image alone would justify.
    #[derive(Clone, Copy, PartialEq, Debug)]
    enum Ack {
        Durable,
        Staged,
    }

    /// Group commit, the A/B copies, and the startup scan. There is no journal, so
    /// `rebuild` is the entire recovery procedure and this is the only check on it.
    #[derive(Clone, PartialEq, Hash, Debug)]
    struct Disk {
        shards: Vec<Shard>,
        /// Both copies of every mblock. `None` is a copy a failed write destroyed.
        image: Vec<Copies>,
        pending: Vec<(u64, Ticket)>,
        /// Staged but not durable: what to report once `durable_seq` catches up, exactly
        /// what `flush_until` waits on.
        waiting: Vec<Waiting>,
        acked: BTreeSet<(u64, u64, u32, u32)>,
        /// The flush in flight: mblock, the sequence it makes durable, its generation and
        /// the rows it carries.
        flight: Option<(u32, u64, u64, Vec<Entry>)>,
        /// Both copies of one mblock died: genuine media loss, answered with `Missing`.
        /// Sticky, since a later successful write refills a copy but not the lost rows.
        lost: bool,
        writes: u8,
        flushes: u8,
        restarts: u8,
        cores: u32,
    }

    #[derive(Clone, Copy, PartialEq, Debug)]
    enum DiskAct {
        Reserve { a: u8, b: u8 },
        Stage(u8),
        FlushGo(u8),
        FlushDone(bool),
        Restart(u8),
    }

    struct DiskModel {
        ab: Ab,
        ack: Ack,
        commit: Commit,
    }

    impl DiskModel {
        /// Move everything whose mblock has reached its sequence into `acked`, which is
        /// `flush_until`'s predicate.
        fn promote(&self, s: &mut Disk) {
            if self.ack == Ack::Staged {
                return;
            }
            let durable = s.shards[0].slab(Class::Small).durable_seq.to_vec();
            let acked = &mut s.acked;
            let mut stale = Vec::new();
            s.waiting.retain(|&(a, v, b, val, li, seq, old)| {
                if durable[li as usize] >= seq {
                    acked.insert((a, v, b, val));
                    stale.extend(old);
                    false
                } else {
                    true
                }
            });
            maps!(m);
            for slot in stale {
                s.shards[0].release(Class::Small, slot, &m);
            }
        }
    }

    impl Model for DiskModel {
        type State = Disk;
        type Action = DiskAct;

        fn init_states(&self) -> Vec<Disk> {
            let image = formatted();
            vec![Disk {
                shards: boot(1, self.commit == Commit::Checked, &image).0,
                image,
                pending: Vec::new(),
                waiting: Vec::new(),
                acked: BTreeSet::new(),
                flight: None,
                lost: false,
                writes: 0,
                flushes: 0,
                restarts: 0,
                cores: 1,
            }]
        }

        fn actions(&self, s: &Disk, out: &mut Vec<DiskAct>) {
            // The model drives core 0 only, so writes stop once a restart has spread the
            // shards wider. That restart is there for `rebuild`, not for traffic.
            if s.cores == 1 && s.writes < MAX_WRITES && s.pending.len() < MAX_PENDING {
                for a in 0..ADDRS.len() as u8 {
                    for b in 0..2u8 {
                        out.push(DiskAct::Reserve { a, b });
                    }
                }
            }
            for p in 0..s.pending.len() as u8 {
                out.push(DiskAct::Stage(p));
            }
            if s.flight.is_none() {
                if s.flushes < MAX_FLUSHES {
                    for li in 0..s.shards[0].slabs[0].local as u8 {
                        out.push(DiskAct::FlushGo(li));
                    }
                }
            } else {
                out.push(DiskAct::FlushDone(true));
                out.push(DiskAct::FlushDone(false));
            }
            if s.restarts < 1 {
                out.push(DiskAct::Restart(1));
                out.push(DiskAct::Restart(2));
            }
        }

        fn next_state(&self, last: &Disk, act: DiskAct) -> Option<Disk> {
            let mut s = last.clone();
            let cls = Class::Small;
            match act {
                DiskAct::Reserve { a, b } => {
                    let ad = GlobalAddr(ADDRS[a as usize]);
                    let ballot = Ballot::new(BALLOTS[b as usize].0, BALLOTS[b as usize].1);
                    let t = s.shards[0]
                        .reserve(ad, Kind::Lww, cls, None, ballot, 0)
                        .ok()?;
                    s.pending.push((ad.0, t));
                    s.writes += 1;
                }
                DiskAct::Stage(p) => {
                    let (addr, t) = s.pending.remove(p as usize);
                    let value = t.ballot.raw();
                    maps!(m);
                    let st = s.shards[0]
                        .stage(GlobalAddr(addr), Kind::Lww, cls, t, value, &m)
                        .ok()??;
                    match self.ack {
                        Ack::Staged => {
                            s.acked.insert((addr, t.version, t.ballot.raw(), value));
                            if let Some(old) = st.stale {
                                s.shards[0].release(cls, old, &m);
                            }
                        }
                        Ack::Durable => s.waiting.push((
                            addr,
                            t.version,
                            t.ballot.raw(),
                            value,
                            st.li,
                            st.seq,
                            st.stale,
                        )),
                    }
                }
                DiskAct::FlushGo(li) => {
                    let li = li as u32;
                    let need = s.shards[0].slab(cls).commit_seq[li as usize];
                    if s.shards[0].flush_act(cls, li, need) != Act::Go {
                        return None;
                    }
                    let (seq, h, rows) = s.shards[0].begin_flush(cls, li);
                    s.flight = Some((li, seq, h.generation, rows.to_vec()));
                    s.flushes += 1;
                }
                DiskAct::FlushDone(ok) => {
                    let (li, seq, g, rows) = s.flight.take()?;
                    let copy = match self.ab {
                        Ab::Alternating => (g % 2) as usize,
                        Ab::Fixed => 0,
                    };
                    // A failed write leaves the copy it aimed at unreadable, which is why
                    // there are two.
                    s.image[li as usize][copy] = if ok { Some((g, rows)) } else { None };
                    s.lost |= s.image[li as usize].iter().all(|c| c.is_none());
                    s.shards[0].end_flush(cls, li, seq, ok);
                    self.promote(&mut s);
                }
                DiskAct::Restart(cores) => {
                    let (shards, _) = boot(cores as u32, self.commit == Commit::Checked, &s.image);
                    s.shards = shards;
                    s.cores = cores as u32;
                    s.restarts += 1;
                    // A crash loses every reservation and every unacknowledged commit.
                    s.pending.clear();
                    s.waiting.clear();
                    s.flight = None;
                }
            }
            Some(s)
        }

        fn properties(&self) -> Vec<Property<Self>> {
            vec![
                // What was reported done is still there. An mblock whose copies both died
                // is genuine media loss, answered with `Missing`, so it is exempt.
                Property::<Self>::always("acknowledged writes survive", |_, s: &Disk| {
                    if s.lost {
                        return true;
                    }
                    // Two ballots may legitimately hold one version and the higher wins,
                    // so the test is on `(version, ballot)` order rather than the value:
                    // never lost, never regressed, and equal keys must agree on the bytes.
                    s.acked
                        .iter()
                        .all(|&(a, v, b, val)| match find(&s.shards, a) {
                            Some(e) => {
                                let held = (e.version, e.ballot as u32);
                                held > (v, b) || (held == (v, b) && e.data_crc == val)
                            }
                            None => false,
                        })
                }),
                Property::<Self>::always("slots are accounted for", |_, s: &Disk| sound(&s.shards)),
                // `rebuild` seeds the census in one pass and `set` keeps it from there. A
                // restart is the only thing that exercises the seeding.
                Property::<Self>::always("the census matches the slabs", |_, s: &Disk| {
                    s.shards
                        .iter()
                        .all(|sh| sh.slabs[0].census == recount(&sh.slabs[0]))
                }),
                // A displaced slot is retired only once its replacement is durable, so
                // until then one address legitimately occupies two slots. Every extra slot
                // must be one a commit still holds; otherwise nothing gives it back.
                Property::<Self>::always("displaced slots are owned", |_, s: &Disk| {
                    let held = s.waiting.iter().filter_map(|w| w.6).collect();
                    duplicates_are_owned(&s.shards, &held)
                }),
                Property::<Self>::sometimes("a write survives a restart", |_, s: &Disk| {
                    s.restarts > 0 && !s.acked.is_empty() && find(&s.shards, ADDRS[0]).is_some()
                }),
                // The older copy is readable exactly when the newer one is not, which is a
                // lost write rather than corruption.
                Property::<Self>::sometimes("the older copy is used", |_, s: &Disk| {
                    s.image.iter().any(|c| c[0].is_none() != c[1].is_none())
                }),
                // Two slots claiming one address, what a crash between the metadata write
                // and the old slot's release leaves behind.
                Property::<Self>::sometimes("a duplicate is resolved at startup", |_, s: &Disk| {
                    s.restarts > 0
                        && s.image
                            .iter()
                            .flatten()
                            .flatten()
                            .filter(|(_, rows)| rows.iter().any(|e| e.addr == ADDRS[0]))
                            .count()
                            > 1
                        && find(&s.shards, ADDRS[0]).is_some()
                }),
                // The index shards by consensus group, the slots by core, and after a core
                // count change those need not agree.
                Property::<Self>::sometimes("a foreign entry appears", |_, s: &Disk| {
                    s.shards.iter().any(|sh| !sh.slabs[0].foreign.is_empty())
                }),
            ]
        }
    }

    // --- checks ---
    //
    // Full-enumeration proofs use `spawn_dfs`: breadth first holds the entire frontier,
    // and each queued item carries a cloned state plus its action path, which for the
    // register models is tens of gigabytes of live heap. Checks that assert a
    // counterexample stay on `spawn_bfs`, the only search that returns the shortest one,
    // and they assert on its length.

    fn threads() -> usize {
        std::thread::available_parallelism()
            .map(|n| n.get())
            .unwrap_or(1)
    }

    /// Stop the search the moment `name` has a counterexample. The checker's default is
    /// to keep going until every property has a discovery, which for a `sometimes`
    /// property that never fires means enumerating the whole state space for nothing. BFS
    /// still hands back the shortest path.
    fn until(name: &'static str) -> HasDiscoveries {
        HasDiscoveries::AnyOf([name].into_iter().collect())
    }

    #[test]
    fn the_occ_pool_evicts_the_coldest_record() {
        let mut r = OccRing::new(2);
        r.observe(1, 10);
        r.observe(2, 20);
        r.observe(1, 11); // re-read moves 1 to the back, ahead of 2
        r.observe(3, 30);
        assert_eq!(
            r.version(1),
            Some(11),
            "a re-read page was evicted before a colder one"
        );
        assert_eq!(r.version(2), None);
        assert_eq!(r.version(3), Some(30));
        // Both halves stay within the bound no matter how the reads repeat.
        for i in 0..64u64 {
            r.observe(i % 5, i);
            assert!(r.seen.len() <= r.cap && r.fifo.len() <= r.cap);
        }
        r.set_cap(1);
        assert_eq!(r.seen.len(), 1);
    }

    /// The three page kinds are three separate state spaces. One test each, so the
    /// harness runs them side by side.
    fn register_semantics(kind: Kind) {
        RegModel {
            kind,
            commit: Commit::Checked,
        }
        .checker()
        .threads(threads())
        .spawn_dfs()
        .join()
        .assert_properties();
    }

    #[test]
    fn lww_register_semantics() {
        register_semantics(Kind::Lww);
    }

    #[test]
    fn occ_register_semantics() {
        register_semantics(Kind::Occ);
    }

    #[test]
    fn immutable_register_semantics() {
        register_semantics(Kind::Immutable);
    }

    #[test]
    fn durability() {
        DiskModel {
            ab: Ab::Alternating,
            ack: Ack::Durable,
            commit: Commit::Checked,
        }
        .checker()
        .threads(threads())
        .spawn_dfs()
        .join()
        .assert_properties();
    }

    /// A reservation and its commit are separated by the data write, so a second accept
    /// for the same address can be granted and committed in between. Installing the ticket
    /// without re-reading the entry lets the loser of that race land last and pull the
    /// register back to an older ballot. This is the check `stage` makes.
    #[test]
    fn blind_commit_regresses_a_register() {
        let path = DiskModel {
            ab: Ab::Alternating,
            ack: Ack::Durable,
            commit: Commit::Blind,
        }
        .checker()
        .threads(threads())
        .finish_when(until("acknowledged writes survive"))
        .spawn_bfs()
        .join()
        .assert_any_discovery("acknowledged writes survive");
        assert!(path.into_actions().len() >= 4);
    }

    /// Acknowledging a commit the moment it is staged is the mistake group commit exists
    /// to avoid: the entry is in DRAM, the mblock is not on the device, a crash takes it.
    #[test]
    fn staged_is_not_durable() {
        let path = DiskModel {
            ab: Ab::Alternating,
            ack: Ack::Staged,
            commit: Commit::Checked,
        }
        .checker()
        .threads(threads())
        .finish_when(until("acknowledged writes survive"))
        .spawn_bfs()
        .join()
        .assert_any_discovery("acknowledged writes survive");
        assert!(path.into_actions().len() >= 3);
    }

    /// Writing every generation to the same copy makes a failed metadata write
    /// destructive: the previous contents are gone, and with them an acknowledged value
    /// unrelated to the write that failed.
    #[test]
    fn one_copy_is_not_enough() {
        let path = DiskModel {
            ab: Ab::Fixed,
            ack: Ack::Durable,
            commit: Commit::Checked,
        }
        .checker()
        .threads(threads())
        .finish_when(until("acknowledged writes survive"))
        .spawn_bfs()
        .join()
        .assert_any_discovery("acknowledged writes survive");
        assert!(path.into_actions().len() >= 3);
    }
}

#[cfg(test)]
mod lending {
    use super::*;

    /// A slab of `n` slots, every one of them free.
    fn slab(n: u32) -> Slab {
        let mut sl = Slab::new(0, 1, n, 1, n as u64);
        sl.free = (0..n).rev().collect();
        sl
    }

    /// A loan is not a commitment: the allocator can recall any lent slot inside a
    /// reservation, so lending must not move the pressure watermarks.
    #[test]
    fn a_loan_still_counts_as_free() {
        let mut sl = slab(100);
        assert_eq!(sl.capacity(), (100, 100));
        for _ in 0..10 {
            assert!(sl.lend().is_some());
        }
        assert_eq!(sl.free.len(), 90);
        assert_eq!(sl.capacity(), (100, 100), "a loan is still free capacity");
        assert_eq!(sl.pressure(), Pressure::Normal);
    }

    /// Lending stops well short of the slab, so reclaim never stands between an ordinary
    /// write and a slot. The gate reads `free`, not `free + lent`, which keeps lending
    /// from walking past its own limit.
    #[test]
    fn lending_stops_at_a_quarter_of_the_stripe() {
        let mut sl = slab(100);
        let mut n = 0;
        while sl.lend().is_some() {
            n += 1;
            assert!(n <= 100, "lending must terminate");
        }
        assert_eq!(n, 75);
        assert_eq!(sl.free.len(), 25);
        assert_eq!(sl.pressure(), Pressure::Normal);
    }

    /// A slab that is already short lends nothing at all.
    #[test]
    fn a_pressed_slab_lends_nothing() {
        let mut sl = slab(100);
        sl.free.truncate(1);
        assert_eq!(sl.pressure(), Pressure::Low);
        assert!(sl.lend().is_none());
    }

    #[test]
    fn reclaim_takes_back_exactly_what_was_lent() {
        let mut sl = slab(100);
        let l = sl.lend().unwrap();
        assert!(!sl.free.contains(&l));
        assert!(sl.reclaim(l));
        assert!(sl.free.contains(&l));
        assert_eq!(sl.free.len(), 100);
        // A repeat is a no-op rather than a double free: the cache may hand back an
        // offset for a chunk it has already given up.
        assert!(!sl.reclaim(l));
        assert_eq!(sl.free.len(), 100);
    }
}
