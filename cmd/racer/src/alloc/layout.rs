//! On-disk formats: codec and arithmetic, plus the format and read-back path at the
//! bottom, which runs on the control thread with plain syscalls.
//!
//! ```text
//! +-----------+--------+-----------------------------+-------------------------+-------------+
//! | Superblk  | zeroes |      Metadata region        |       Data region       |    Tail     |
//! | x4 copies |  4 MiB | mut A|B | immut A|B         | mut slab   | immut slab | 4 MiB chunks|
//! +-----------+--------+-----------------------------+-------------------------+-------------+
//!                       <---------- authoritative, mblock-backed -------------> <-no metadata->
//!                                                                    alloc_end -^
//! ```
//!
//! The 4 MiB of zeroes belongs to no region. A read of a never-written block DMAs from it.
//!
//! The tail is everything between `alloc_end` and the end of the store: derived by
//! [`Geometry::tail`] at every open, named by no superblock word, never persisted. The
//! cache lives there and is volatile, so `grow` shrinks the tail without moving a byte.

use std::io;
use std::path::Path;
use std::sync::Arc;

use crate::config::GroupId;
use crate::runtime::Limiter;

mod crc;
mod device;

#[cfg(test)]
use crc::crc32c_sw;
use crc::crc32c_with;
pub use crc::{crc32c, page_crc};
pub use device::size_if_needed;
use device::sync;
pub(crate) use device::{Aligned, Dev, open_direct, read_at, write_at};

pub(crate) const MBLOCK: usize = 4096;
const MB_HDR: usize = 64;
/// Entry widths. Only the mutable entry carries `data_crc`: an immutable block is written
/// once and never rewritten, so it is not checksummed.
const ENTRY_MUTABLE: usize = 36;
const ENTRY_IMMUTABLE: usize = 32;
/// Slots per mblock. Both are exact fits: 64 + 112*36 = 64 + 126*32 = 4096.
const K_MUTABLE: u32 = 112;
const K_IMMUTABLE: u32 = 126;

/// The block both classes are made of. Every slot in either data slab is one of these.
pub(crate) const SMALL_PAGE: u64 = 4096;
/// The placement stripe immutable blocks are grouped by, and the alignment the tail and
/// the data slabs are carved on.
pub(crate) const HUGE_PAGE: u64 = 4 << 20;

const SB_MAGIC: u32 = 0x5243_5342; // "RCSB"
const MB_MAGIC: u32 = 0x524d_4232; // "RMB2"
/// On-disk format version and the only compatibility guard: no older format is detectable
/// from the bytes, so an older store must be reformatted. v3 brought the
/// `universe:26 | lba:38` address and extent-id seals, v4 a grown layout, v5 the tail, and
/// v6 made both classes 4 KiB so the class tag names mutability rather than page width.
const FMT_VER: u16 = 6;
const SB_COPIES: u64 = 4;
const SB_REGION: u64 = SB_COPIES * MBLOCK as u64;
/// Superblock tail reserved for the consensus record; geometry must not reach into it.
const SB_RESERVED: usize = 3072;

/// The unit the tail is carved into: 1024 cache slots, 4 MiB aligned so it satisfies
/// O_DIRECT.
pub(crate) const CHUNK_BYTES: u64 = HUGE_PAGE;

/// Both classes in the order their ids are tagged with on disk.
const CLASSES: [Class; 2] = [Class::Mutable, Class::Immutable];

fn ver_ok(v: u16) -> bool {
    v == FMT_VER
}

/// Which slab a block lives in, which is the extent's kind. Both slabs hand out 4 KiB
/// slots; the class decides whether the block is checksummed, whether it may be cached,
/// and which guard a write is held to. It is persisted next to the row so a row whose
/// extent has left the config can still be placed and shed.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Class {
    /// Overwritten under optimistic concurrency, checksummed, never cached.
    Mutable,
    /// Write-once per tombstone epoch, unchecksummed, cacheable.
    Immutable,
}

impl Class {
    /// The class an extent's kind puts a block in.
    pub(crate) fn of(kind: crate::config::Kind) -> Class {
        match kind {
            crate::config::Kind::Mutable => Class::Mutable,
            crate::config::Kind::Immutable => Class::Immutable,
        }
    }

    /// The kind this class stores, for placing a row the config no longer covers.
    pub(crate) fn kind(self) -> crate::config::Kind {
        match self {
            Class::Mutable => crate::config::Kind::Mutable,
            Class::Immutable => crate::config::Kind::Immutable,
        }
    }

    /// Whether a block in this class carries an authoritative data checksum.
    pub(crate) fn checksummed(self) -> bool {
        self == Class::Mutable
    }

    pub(crate) fn bytes(self) -> u64 {
        SMALL_PAGE
    }

    fn entry_bytes(self) -> usize {
        match self {
            Class::Mutable => ENTRY_MUTABLE,
            Class::Immutable => ENTRY_IMMUTABLE,
        }
    }

    /// Slots per mblock.
    pub(crate) fn k(self) -> u32 {
        match self {
            Class::Mutable => K_MUTABLE,
            Class::Immutable => K_IMMUTABLE,
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug, Default)]
#[repr(u8)]
pub(crate) enum State {
    #[default]
    Empty = 0,
    Live = 1,
    /// Written then trimmed; the entry survives to tell a trim from a hole (CORFU).
    Tombstone = 2,
}

/// One decoded mblock entry, and the in-DRAM slot record. The image is authoritative in
/// memory, so a metadata write is a full-block rewrite, never RMW.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug, Default)]
pub(crate) struct Entry {
    /// The page in this slot. Zero means free, which is why universe id 0 is reserved.
    pub(crate) addr: u64,
    pub(crate) version: u64,
    /// CASPaxos accepted ballot (`paxos::Ballot`), in the low 32 bits.
    pub(crate) ballot: u64,
    /// CRC32C of the page seeded with `(addr, version)`; only for `Live` `Small` entries.
    pub(crate) data_crc: u32,
    /// Tombstone epoch, for the tombstone sweep.
    pub(crate) epoch: u32,
    pub(crate) state: State,
    pub(crate) flags: u8,
}

impl Entry {
    fn decode(class: Class, b: &[u8]) -> Entry {
        let state = match b[if class == Class::Mutable { 28 } else { 24 }] {
            1 => State::Live,
            2 => State::Tombstone,
            _ => State::Empty,
        };
        let (flags, epoch, crc) = match class {
            Class::Mutable => (b[29], u32(&b[32..]), u32(&b[24..])),
            Class::Immutable => (b[25], u32(&b[28..]), 0),
        };
        Entry {
            addr: u64f(&b[0..]),
            version: u64f(&b[8..]),
            ballot: u64f(&b[16..]),
            data_crc: crc,
            epoch,
            state,
            flags,
        }
    }

    fn encode(&self, class: Class, b: &mut [u8]) {
        b[0..8].copy_from_slice(&self.addr.to_le_bytes());
        b[8..16].copy_from_slice(&self.version.to_le_bytes());
        b[16..24].copy_from_slice(&self.ballot.to_le_bytes());
        match class {
            Class::Mutable => {
                b[24..28].copy_from_slice(&self.data_crc.to_le_bytes());
                b[28] = self.state as u8;
                b[29] = self.flags;
                b[30..32].fill(0);
                b[32..36].copy_from_slice(&self.epoch.to_le_bytes());
            }
            Class::Immutable => {
                b[24] = self.state as u8;
                b[25] = self.flags;
                b[26..28].fill(0);
                b[28..32].copy_from_slice(&self.epoch.to_le_bytes());
            }
        }
    }
}

// ---------------------------------------------------------------------------- mblock

/// Decoded mblock header; a slot index is positional, fixed by `mblock_id` and the class.
#[derive(Clone, Copy, Debug)]
pub(crate) struct Header {
    pub(crate) mblock_id: u32,
    pub(crate) generation: u64,
    pub(crate) class: Class,
    pub(crate) live: u16,
}

/// Offset of the header CRC, computed over everything else in the block.
const CRC_OFF: usize = 12;

/// Serialise a whole mblock. `entries` must have exactly `class.k()` elements.
pub(crate) fn put_mblock(buf: &mut [u8], h: Header, entries: &[Entry]) {
    debug_assert_eq!(buf.len(), MBLOCK);
    debug_assert_eq!(entries.len(), h.class.k() as usize);
    buf.fill(0);
    buf[0..4].copy_from_slice(&MB_MAGIC.to_le_bytes());
    buf[4..6].copy_from_slice(&FMT_VER.to_le_bytes());
    buf[6] = h.class as u8;
    buf[8..12].copy_from_slice(&h.mblock_id.to_le_bytes());
    buf[16..24].copy_from_slice(&h.generation.to_le_bytes());
    buf[24..26].copy_from_slice(&h.live.to_le_bytes());
    let w = h.class.entry_bytes();
    for (i, e) in entries.iter().enumerate() {
        e.encode(h.class, &mut buf[MB_HDR + i * w..MB_HDR + (i + 1) * w]);
    }
    let crc = mblock_crc(buf);
    buf[CRC_OFF..CRC_OFF + 4].copy_from_slice(&crc.to_le_bytes());
}

/// Validate and decode an mblock header. `None` on wrong magic, wrong format, or bad CRC.
pub(crate) fn get_header(buf: &[u8]) -> Option<Header> {
    if buf.len() != MBLOCK
        || u32(&buf[0..]) != MB_MAGIC
        || u16(&buf[4..]) != FMT_VER
        || u32(&buf[CRC_OFF..]) != mblock_crc(buf)
    {
        return None;
    }
    let class = match buf[6] {
        0 => Class::Mutable,
        1 => Class::Immutable,
        _ => return None,
    };
    Some(Header {
        mblock_id: u32(&buf[8..]),
        generation: u64f(&buf[16..]),
        class,
        live: u16(&buf[24..]),
    })
}

pub(crate) fn get_entry(buf: &[u8], class: Class, i: u32) -> Entry {
    let w = class.entry_bytes();
    Entry::decode(class, &buf[MB_HDR + i as usize * w..])
}

/// Everything except the four CRC bytes themselves.
fn mblock_crc(buf: &[u8]) -> u32 {
    crc32c_with(crc32c(&buf[..CRC_OFF]), &buf[CRC_OFF + 4..])
}

// ------------------------------------------------------------------------- superblock

/// One contiguous run of mblocks for one class: metadata copy A, copy B a whole run later,
/// and the data slots they name. `grow` appends past the end, so nothing there moves.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
struct Extent {
    meta: u64,
    data: u64,
    mblocks: u64,
}

/// Extents per class, extent 0 included; each at least doubles the class, so 8 is ample.
const MAX_EXT: usize = 8;

/// Where every region starts and how big it is. Set at format time, carried in the
/// superblock, changed only by `grow`. The cache is absent: see [`Geometry::tail`].
#[derive(Clone, Copy, Debug, PartialEq, Default)]
pub struct Geometry {
    /// First byte past everything the layout owns; `grow` places from here.
    alloc_end: u64,
    mutable: [Extent; MAX_EXT],
    immutable: [Extent; MAX_EXT],
    n_mutable: u8,
    n_immutable: u8,
}

/// Spare slots beyond the config's block count, for in-flight out-of-place writes and
/// tombstones: 5% plus a `floor` so a tiny class still has somewhere to land a retry.
fn overprovision(blocks: u64, floor: u64) -> u64 {
    if blocks == 0 {
        0
    } else {
        blocks + blocks / 20 + floor
    }
}

fn align_up(x: u64, a: u64) -> u64 {
    x.div_ceil(a) * a
}

/// The geometry words: extent 0 of each class, and where the layout ends.
const WORDS: usize = 9;
/// The growth table, between the geometry words and the consensus record.
const GT_MAGIC: u32 = 0x5247_5854; // "RGXT"
const GT_OFF: usize = 16 + WORDS * 8;
const GT_HDR: usize = 16;
const EXT_BYTES: usize = 32;
const GT_ROWS: usize = CLASSES.len() * (MAX_EXT - 1);
const _: () = assert!(GT_OFF + GT_HDR + GT_ROWS * EXT_BYTES <= MBLOCK - SB_RESERVED);

impl Geometry {
    /// Size every region from the config and the store. Fails if the store is too small.
    fn plan(store_bytes: u64, cfg: &crate::config::Config) -> io::Result<Geometry> {
        let g = Geometry::place(claim(store_bytes, cfg));

        g.check(store_bytes)?;
        Ok(g)
    }

    /// Where every region goes, given the mblocks each class wants. Reads nothing: the
    /// layout is a function of the config alone, which is what lets [`store_floor`] answer
    /// how big a store has to be before there is one.
    fn place(want: [u64; 2]) -> Geometry {
        let mut g = Geometry::default();

        let mut at = SB_REGION;
        // Both metadata regions together at the head of the store, then both data regions.
        let meta = [at, at + want[0] * 2 * MBLOCK as u64];
        at = meta[1] + want[1] * 2 * MBLOCK as u64;
        for (i, class) in CLASSES.into_iter().enumerate() {
            at = align_up(at, class.bytes());
            let e = Extent {
                meta: meta[i],
                data: at,
                mblocks: want[i],
            };
            at += e.mblocks * class.k() as u64 * class.bytes();
            g.push(class, e).expect("first extent of an empty geometry");
        }
        g.alloc_end = at;
        g
    }

    fn extents(&self, class: Class) -> &[Extent] {
        match class {
            Class::Mutable => &self.mutable[..self.n_mutable as usize],
            Class::Immutable => &self.immutable[..self.n_immutable as usize],
        }
    }

    /// Append a run. `None` once a class has been grown `MAX_EXT` times.
    fn push(&mut self, class: Class, e: Extent) -> Option<()> {
        let (a, n) = match class {
            Class::Mutable => (&mut self.mutable, &mut self.n_mutable),
            Class::Immutable => (&mut self.immutable, &mut self.n_immutable),
        };
        *a.get_mut(*n as usize)? = e;
        *n += 1;
        Some(())
    }

    /// Place `n` more mblocks past the end of everything and move `alloc_end`. Reads
    /// nothing but the layout, so a growth cut short by a crash is re-placed identically.
    fn append(&mut self, class: Class, n: u64) -> io::Result<()> {
        let meta = self.alloc_end;
        let data = align_up(meta + n * 2 * MBLOCK as u64, class.bytes());
        self.alloc_end = data + n * class.k() as u64 * class.bytes();
        self.push(
            class,
            Extent {
                meta,
                data,
                mblocks: n,
            },
        )
        .ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("store has already been grown {} times", MAX_EXT - 1),
            )
        })
    }

    /// The run holding mblock `id`, and the id of that run's first block.
    fn ext_of(&self, class: Class, id: u64) -> Option<(&Extent, u64)> {
        let mut first = 0;
        for e in self.extents(class) {
            if id < first + e.mblocks {
                return Some((e, first));
            }
            first += e.mblocks;
        }
        None
    }

    pub(crate) fn mblocks(&self, class: Class) -> u64 {
        self.extents(class).iter().map(|e| e.mblocks).sum()
    }

    pub(crate) fn slots(&self, class: Class) -> u64 {
        self.mblocks(class) * class.k() as u64
    }

    /// The first mblock id past the run holding `id`; a batched metadata read stops here.
    pub(crate) fn ext_end(&self, class: Class, id: u64) -> u64 {
        self.ext_of(class, id)
            .map_or(id, |(e, first)| first + e.mblocks)
    }

    /// Byte offset of a data slot.
    pub(crate) fn slot_off(&self, class: Class, slot: u32) -> u64 {
        let k = class.k() as u64;
        let (e, first) = self
            .ext_of(class, slot as u64 / k)
            .expect("slot is within the geometry");
        e.data + (slot as u64 - first * k) * class.bytes()
    }

    /// First byte past everything the layout owns.
    pub(crate) fn alloc_end(&self) -> u64 {
        self.alloc_end
    }

    /// The cache tail: base offset and length, in whole 4 MiB chunks, between the end of
    /// the layout and the end of the store. Derived at every open, never written down.
    pub(crate) fn tail(&self, store_bytes: u64) -> (u64, u64) {
        let base = align_up(self.alloc_end(), CHUNK_BYTES);
        let end = store_bytes / CHUNK_BYTES * CHUNK_BYTES;
        (base, end.saturating_sub(base))
    }

    /// 4 MiB chunks the tail holds.
    pub(crate) fn tail_chunks(&self, store_bytes: u64) -> u64 {
        self.tail(store_bytes).1 / CHUNK_BYTES
    }

    /// Byte offset of one copy of an mblock. Copies A and B sit a whole run apart, not
    /// adjacent, so one bad neighborhood cannot take both copies of a block.
    pub(crate) fn mblock_off(&self, class: Class, id: u32, copy: u8) -> u64 {
        let (e, first) = self
            .ext_of(class, id as u64)
            .expect("mblock id is within the geometry");
        e.meta + (copy as u64 * e.mblocks + (id as u64 - first)) * MBLOCK as u64
    }

    /// The two limits a layout has to stay inside, checked wherever one is built.
    fn check(&self, store_bytes: u64) -> io::Result<()> {
        if self.alloc_end > store_bytes {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!(
                    "config needs {} B, node.store.size_bytes is {store_bytes} B",
                    self.alloc_end
                ),
            ));
        }
        // Slot and mblock ids are u32 everywhere: wire, free lists, and mblock header.
        for class in CLASSES {
            if self.slots(class) > u32::MAX as u64 {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!(
                        "{} slots is past what a 32-bit slot id can name",
                        self.slots(class)
                    ),
                ));
            }
        }
        Ok(())
    }

    /// Write the geometry into a read superblock image, keeping consensus, refreshing CRC.
    fn patch(&self, b: &mut [u8]) {
        b[..CS_OFF].fill(0);
        b[0..4].copy_from_slice(&SB_MAGIC.to_le_bytes());
        b[4..6].copy_from_slice(&FMT_VER.to_le_bytes());
        b[6..8].copy_from_slice(&(K_MUTABLE as u16).to_le_bytes());
        b[8..10].copy_from_slice(&(K_IMMUTABLE as u16).to_le_bytes());
        for (i, v) in self.words().iter().enumerate() {
            b[16 + i * 8..24 + i * 8].copy_from_slice(&v.to_le_bytes());
        }
        self.save_growth(b);
        let crc = crc32c(&b[..MBLOCK - 4]);
        b[MBLOCK - 4..].copy_from_slice(&crc.to_le_bytes());
    }

    fn encode(&self, b: &mut [u8]) {
        b.fill(0);
        self.patch(b);
    }

    fn decode(b: &[u8]) -> Option<Geometry> {
        if b.len() != MBLOCK
            || u32(&b[0..]) != SB_MAGIC
            || !ver_ok(u16(&b[4..]))
            || u16(&b[6..]) as u32 != K_MUTABLE
            || u16(&b[8..]) as u32 != K_IMMUTABLE
            || u32(&b[MBLOCK - 4..]) != crc32c(&b[..MBLOCK - 4])
        {
            return None;
        }
        let w: [u64; WORDS] = std::array::from_fn(|i| u64f(&b[16 + i * 8..]));
        // Words 6 and 7 are extent 0's slot counts, redundant with 2 and 3; check them.
        if w[6] != w[2] * K_MUTABLE as u64 || w[7] != w[3] * K_IMMUTABLE as u64 {
            return None;
        }
        let mut g = Geometry {
            alloc_end: w[8],
            ..Geometry::default()
        };
        g.push(
            Class::Mutable,
            Extent {
                meta: w[0],
                data: w[4],
                mblocks: w[2],
            },
        )?;
        g.push(
            Class::Immutable,
            Extent {
                meta: w[1],
                data: w[5],
                mblocks: w[3],
            },
        )?;
        g.load_growth(b)?;
        Some(g)
    }

    /// Extent 0 of each class plus where the layout ends; grown extents go in the table.
    fn words(&self) -> [u64; WORDS] {
        let (s, h) = (self.mutable[0], self.immutable[0]);
        [
            s.meta,
            h.meta,
            s.mblocks,
            h.mblocks,
            s.data,
            h.data,
            s.mblocks * K_MUTABLE as u64,
            h.mblocks * K_IMMUTABLE as u64,
            self.alloc_end,
        ]
    }

    /// The extents `grow` appended, in id order per class. Always written, even when empty.
    fn save_growth(&self, b: &mut [u8]) {
        let mut at = GT_OFF + GT_HDR;
        let mut n = 0u16;
        for (tag, class) in CLASSES.into_iter().enumerate() {
            for e in self.extents(class).iter().skip(1) {
                b[at..at + 4].copy_from_slice(&(tag as u32).to_le_bytes());
                b[at + 8..at + 16].copy_from_slice(&e.meta.to_le_bytes());
                b[at + 16..at + 24].copy_from_slice(&e.data.to_le_bytes());
                b[at + 24..at + 32].copy_from_slice(&e.mblocks.to_le_bytes());
                at += EXT_BYTES;
                n += 1;
            }
        }
        b[GT_OFF..GT_OFF + 4].copy_from_slice(&GT_MAGIC.to_le_bytes());
        b[GT_OFF + 4..GT_OFF + 6].copy_from_slice(&FMT_VER.to_le_bytes());
        b[GT_OFF + 6..GT_OFF + 8].copy_from_slice(&n.to_le_bytes());
    }

    fn load_growth(&mut self, b: &[u8]) -> Option<()> {
        let n = u16(&b[GT_OFF + 6..]) as usize;
        if u32(&b[GT_OFF..]) != GT_MAGIC || u16(&b[GT_OFF + 4..]) != FMT_VER || n > GT_ROWS {
            return None;
        }
        for i in 0..n {
            let at = GT_OFF + GT_HDR + i * EXT_BYTES;
            let class = CLASSES[(u32(&b[at..]) as usize).min(CLASSES.len() - 1)];
            self.push(
                class,
                Extent {
                    meta: u64f(&b[at + 8..]),
                    data: u64f(&b[at + 16..]),
                    mblocks: u64f(&b[at + 24..]),
                },
            )?;
        }
        Some(())
    }
}

/// Mblocks each class needs for `cfg`; `plan` and `grow` share this rounding.
fn wanted(cfg: &crate::config::Config) -> [u64; 2] {
    [
        overprovision(cfg.mutable_blocks(), 64).div_ceil(K_MUTABLE as u64),
        overprovision(cfg.immutable_blocks(), 64).div_ceil(K_IMMUTABLE as u64),
    ]
}

/// Mblocks each class holds in a store of `store_bytes`, which is exactly what the config
/// asks for.
///
/// Both classes hand out 4 KiB slots and both cost a row in the allocator's DRAM index, so
/// neither can absorb the store's spare: claiming it would spend memory nobody asked for,
/// which is what `policy.max_index_bytes` bounds. What is left over past `alloc_end` is the
/// tail, and the tail is the cache: volatile, indexed under `policy.cache_index_bytes`, and
/// given up without moving a byte when a later `grow` needs the room.
fn claim(store_bytes: u64, cfg: &crate::config::Config) -> [u64; 2] {
    let _ = store_bytes;
    wanted(cfg)
}

/// The smallest store a node declaring these block counts can be formatted on.
///
/// The layout is a function of the config and the store only has to hold it, so a caller
/// that sizes a store for itself can ask rather than guess. A data region rounds up to a
/// whole mblock of slots, which a sparse store is welcome to leave unwritten. Only the
/// simulator sizes its own stores; a real one is whatever the operator handed over.
pub(crate) fn store_floor(mutable_blocks: u64, immutable_blocks: u64) -> u64 {
    Geometry::place([
        overprovision(mutable_blocks, 64).div_ceil(K_MUTABLE as u64),
        overprovision(immutable_blocks, 64).div_ceil(K_IMMUTABLE as u64),
    ])
    .alloc_end()
}

// -------------------------------------------------------------- consensus side state

/// One seal-table row: an extent frozen as a migration source, named by its unique id.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct Seal {
    pub(crate) extent: u32,
    pub(crate) term: u32,
}

/// Consensus state that is not a page register: `promised_term` per group and the seal
/// table, both in the superblock because neither is addressed by page. Losing a seal is the
/// one metadata loss that is not self-healing, so it gets fourfold redundancy.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct Consensus {
    /// `(group, promised_term)` sorted by group; a group is a universe and a catalog index.
    pub(crate) terms: Vec<(GroupId, u32)>,
    pub(crate) seals: Vec<Seal>,
}

const CS_MAGIC: u32 = 0x5250_5831; // "RPX1"
const CS_OFF: usize = 1024;
const CS_HDR: usize = 16;
const TERM_BYTES: usize = 12;
const SEAL_BYTES: usize = 8;
/// Bounded by what fits beside the geometry in one 4 KiB block.
pub(crate) const MAX_TERMS: usize = 128;
pub(crate) const MAX_SEALS: usize = 96;
const _: () =
    assert!(CS_OFF + CS_HDR + MAX_TERMS * TERM_BYTES + MAX_SEALS * SEAL_BYTES <= MBLOCK - 4);
const _: () = assert!(CS_OFF == MBLOCK - SB_RESERVED);

impl Consensus {
    /// Patch this state into a read superblock image, keeping geometry, refreshing the CRC.
    pub(crate) fn patch(&self, b: &mut [u8]) {
        assert!(self.terms.len() <= MAX_TERMS && self.seals.len() <= MAX_SEALS);
        b[CS_OFF..MBLOCK - 4].fill(0);
        let h = &mut b[CS_OFF..CS_OFF + CS_HDR];
        h[0..4].copy_from_slice(&CS_MAGIC.to_le_bytes());
        h[4..6].copy_from_slice(&FMT_VER.to_le_bytes());
        h[6..8].copy_from_slice(&(self.terms.len() as u16).to_le_bytes());
        h[8..10].copy_from_slice(&(self.seals.len() as u16).to_le_bytes());
        let mut at = CS_OFF + CS_HDR;
        for (g, t) in &self.terms {
            b[at..at + 4].copy_from_slice(&g.universe().to_le_bytes());
            b[at + 4..at + 8].copy_from_slice(&g.index().to_le_bytes());
            b[at + 8..at + 12].copy_from_slice(&t.to_le_bytes());
            at += TERM_BYTES;
        }
        for s in &self.seals {
            b[at..at + 4].copy_from_slice(&s.extent.to_le_bytes());
            b[at + 4..at + 8].copy_from_slice(&s.term.to_le_bytes());
            at += SEAL_BYTES;
        }
        let crc = crc32c(&b[..MBLOCK - 4]);
        b[MBLOCK - 4..].copy_from_slice(&crc.to_le_bytes());
    }

    /// Decode from a superblock image whose CRC the caller checked; no magic means empty.
    fn decode(b: &[u8]) -> Consensus {
        let mut c = Consensus::default();
        if b.len() != MBLOCK || u32(&b[CS_OFF..]) != CS_MAGIC || u16(&b[CS_OFF + 4..]) != FMT_VER {
            return c;
        }
        let nt = (u16(&b[CS_OFF + 6..]) as usize).min(MAX_TERMS);
        let ns = (u16(&b[CS_OFF + 8..]) as usize).min(MAX_SEALS);
        let mut at = CS_OFF + CS_HDR;
        for _ in 0..nt {
            c.terms.push((
                GroupId::new(u32(&b[at..]), u32(&b[at + 4..])),
                u32(&b[at + 8..]),
            ));
            at += TERM_BYTES;
        }
        for _ in 0..ns {
            c.seals.push(Seal {
                extent: u32(&b[at..]),
                term: u32(&b[at + 4..]),
            });
            at += SEAL_BYTES;
        }
        c
    }
}

/// Byte offset of superblock copy `i`. Consensus state is rewritten through every copy.
pub(crate) fn sb_off(i: u64) -> u64 {
    i * MBLOCK as u64
}

pub(crate) fn sb_copies() -> u64 {
    SB_COPIES
}

/// Whether a superblock image is intact. Callers check this before `Consensus::decode`.
pub(crate) fn sb_valid(b: &[u8]) -> bool {
    b.len() == MBLOCK
        && u32(&b[0..]) == SB_MAGIC
        && ver_ok(u16(&b[4..]))
        && u32(&b[MBLOCK - 4..]) == crc32c(&b[..MBLOCK - 4])
}

/// Read the consensus side state at startup, trying every copy in turn.
pub(crate) fn read_consensus(path: &Path) -> io::Result<Consensus> {
    let f = open_direct(path, false)?;
    let mut buf = Aligned::new(MBLOCK);
    for i in 0..SB_COPIES {
        if read_at(&f, buf.as_mut(), sb_off(i)).is_ok() && sb_valid(buf.as_ref()) {
            return Ok(Consensus::decode(buf.as_ref()));
        }
    }
    Err(io::Error::new(
        io::ErrorKind::InvalidData,
        "no valid superblock; store is not formatted",
    ))
}

// ----------------------------------------------------------------------------- format

/// Lay the store out for `cfg`, destroying whatever was there.
pub fn format(path: &Path, cfg: &crate::config::Config) -> io::Result<Geometry> {
    // Writing every mblock is the heaviest IO the store sees; meter it like the rest.
    let f = open_direct(path, true)?.meter(Arc::new(Limiter::new(
        cfg.node.max_iops(),
        cfg.node.max_bytes_per_sec(),
    )));
    let g = Geometry::plan(cfg.node.store_bytes(), cfg)?;

    // One reused staging buffer: 4 MiB is a 1024-mblock batch.
    let mut buf = Aligned::new(HUGE_PAGE as usize);

    for class in CLASSES {
        write_empty(&f, &g, class, 0, g.mblocks(class), &mut buf)?;
    }

    // Superblocks last: until they land, the store is not formatted.
    sync(&f)?;
    g.encode(&mut buf.as_mut()[..MBLOCK]);
    for i in 0..SB_COPIES {
        write_at(&f, &buf.as_ref()[..MBLOCK], sb_off(i))?;
    }
    sync(&f)?;
    Ok(g)
}

/// Write empty mblocks for `n` blocks of `class` from `first`, both copies, batched.
fn write_empty(
    f: &Dev,
    g: &Geometry,
    class: Class,
    first: u64,
    n: u64,
    buf: &mut Aligned,
) -> io::Result<()> {
    let empty = vec![Entry::default(); class.k() as usize];
    let per_batch = (HUGE_PAGE as usize / MBLOCK) as u64;
    let mut id = first;
    while id < first + n {
        // One extent at a time: the blocks of the next one are elsewhere.
        let batch = per_batch.min(first + n - id).min(g.ext_end(class, id) - id);
        for i in 0..batch {
            let h = Header {
                mblock_id: (id + i) as u32,
                // Both copies start equal; the resolver breaks ties toward A, so the first
                // real write lands on B at generation 1.
                generation: 0,
                class,
                live: 0,
            };
            let at = i as usize * MBLOCK;
            put_mblock(&mut buf.as_mut()[at..at + MBLOCK], h, &empty);
        }
        let bytes = &buf.as_ref()[..batch as usize * MBLOCK];
        for copy in 0..2 {
            write_at(f, bytes, g.mblock_off(class, id as u32, copy))?;
        }
        id += batch;
    }
    Ok(())
}

/// Format a blank store, or leave an existing valid layout untouched.
pub fn format_if_needed(path: &Path, cfg: &crate::config::Config) -> io::Result<()> {
    size_if_needed(path, cfg)?;
    let f = open_direct(path, false)?;
    let mut buf = Aligned::new(MBLOCK);
    let mut blank = true;
    for i in 0..SB_COPIES {
        read_at(&f, buf.as_mut(), sb_off(i))?;
        if Geometry::decode(buf.as_ref()).is_some() {
            return Ok(());
        }
        blank &= buf.as_ref().iter().all(|&b| b == 0);
    }
    if blank {
        format(path, cfg).map(drop)
    } else {
        Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "no valid superblock",
        ))
    }
}

/// Give `cfg` the slots the store was not formatted for, and claim whatever else the store
/// has room for. Everything new goes past the end of the layout, so no existing offset moves
/// and no existing byte is read or rewritten. Called at startup before the allocator opens,
/// which sizes shards from the geometry.
///
/// Claiming the spare rather than only the shortfall is what keeps a growing zone off this
/// path: a node that has already taken the whole store has nothing left to grow into and
/// needs no restart to fit another volume.
pub fn grow_if_needed(path: &Path, cfg: &crate::config::Config) -> io::Result<()> {
    size_if_needed(path, cfg)?;
    let mut g = read_geometry(path)?;
    let want = claim(cfg.node.store_bytes(), cfg);
    let have: [u64; 2] = CLASSES.map(|c| g.mblocks(c));
    let add = fitted(
        &g,
        std::array::from_fn(|i| want[i].saturating_sub(have[i])),
        cfg.node.store_bytes(),
    );

    let f = open_direct(path, true)?.meter(Arc::new(Limiter::new(
        cfg.node.max_iops(),
        cfg.node.max_bytes_per_sec(),
    )));
    if add != [0, 0] {
        // Place both runs before writing, so a store too small for the result costs no IO.
        for (i, class) in CLASSES.into_iter().enumerate() {
            if add[i] > 0 {
                g.append(class, add[i])?;
            }
        }

        // The file is already at `size_bytes`, so this is the config's own bound.
        g.check(cfg.node.store_bytes())?;

        let mut buf = Aligned::new(HUGE_PAGE as usize);
        for (i, class) in CLASSES.into_iter().enumerate() {
            write_empty(&f, &g, class, have[i], add[i], &mut buf)?;
        }
        // Blocks land before a superblock names them, so an interrupted run is no part of
        // the store and is rewritten at the same offsets next boot.
        sync(&f)?;
    }
    save_geometry(&f, &g)
}

/// Trim the opportunistic part of a growth to what appending can actually place.
///
/// [`claim`] sizes the store's spare against a layout placed from scratch, but growth
/// appends past the end of what is already there: every earlier run's metadata and its
/// alignment padding is still in front, so the same mblock counts end further into the
/// store than a fresh [`Geometry::place`] would put them. The difference is bounded and
/// small, but it is not zero, and a store grown twice is enough to see it.
///
/// Both classes are what the config asked for rather than spare the store happened to
/// have, so a store that cannot hold them is a store too small for this node's share,
/// which `check` is there to say out loud.
fn fitted(g: &Geometry, mut add: [u64; 2], store_bytes: u64) -> [u64; 2] {
    loop {
        let mut trial = *g;
        let mut placed = true;
        for (i, class) in CLASSES.into_iter().enumerate() {
            if add[i] > 0 && trial.append(class, add[i]).is_err() {
                placed = false;
                break;
            }
        }
        if placed && trial.alloc_end() <= store_bytes {
            return add;
        }
        if add[1] > 0 {
            add[1] -= 1;
        } else if add[0] > 0 {
            add[0] -= 1;
        } else {
            return add;
        }
    }
}

/// Write `g` through every superblock copy that does not already carry it, preserving each
/// copy's consensus record. Copy 0 first, because a reader takes the first copy that
/// decodes, so a run cut short leaves the newer geometry in front.
fn save_geometry(f: &Dev, g: &Geometry) -> io::Result<()> {
    let mut sb = Aligned::new(MBLOCK);
    let mut wrote = false;
    for i in 0..SB_COPIES {
        read_at(f, sb.as_mut(), sb_off(i))?;
        if Geometry::decode(sb.as_ref()).as_ref() == Some(g) {
            continue;
        }
        // A copy too damaged to patch is rebuilt whole; the next save restores its record.
        if !sb_valid(sb.as_ref()) {
            sb.as_mut().fill(0);
        }
        g.patch(sb.as_mut());
        write_at(f, sb.as_ref(), sb_off(i))?;
        wrote = true;
    }
    if wrote { sync(f) } else { Ok(()) }
}

/// Blocks this geometry falls short of `cfg`, for the restart-grows metric; 0 if it fits.
pub fn shortfall(g: &Geometry, cfg: &crate::config::Config) -> u64 {
    let want = wanted(cfg);
    CLASSES
        .into_iter()
        .enumerate()
        .map(|(i, c)| want[i].saturating_sub(g.mblocks(c)) * c.k() as u64)
        .sum()
}

/// Read the geometry back, trying every superblock copy in turn.
pub(crate) fn read_geometry(path: &Path) -> io::Result<Geometry> {
    let f = open_direct(path, false)?;
    let mut buf = Aligned::new(MBLOCK);
    for i in 0..SB_COPIES {
        if read_at(&f, buf.as_mut(), sb_off(i)).is_ok()
            && let Some(g) = Geometry::decode(buf.as_ref())
        {
            return Ok(g);
        }
    }
    Err(io::Error::new(
        io::ErrorKind::InvalidData,
        "no valid superblock; store is not formatted",
    ))
}

// ------------------------------------------------------------------------------- misc

fn u16(b: &[u8]) -> u16 {
    u16::from_le_bytes(b[..2].try_into().unwrap())
}
fn u32(b: &[u8]) -> u32 {
    u32::from_le_bytes(b[..4].try_into().unwrap())
}
fn u64f(b: &[u8]) -> u64 {
    u64::from_le_bytes(b[..8].try_into().unwrap())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn entries_fill_the_block_exactly() {
        assert_eq!(MB_HDR + K_MUTABLE as usize * ENTRY_MUTABLE, MBLOCK);
        assert_eq!(MB_HDR + K_IMMUTABLE as usize * ENTRY_IMMUTABLE, MBLOCK);
    }

    #[test]
    fn crc32c_matches_known_vectors() {
        assert_eq!(crc32c(b""), 0);
        assert_eq!(crc32c(b"123456789"), 0xe306_9283);
        assert_eq!(
            crc32c(b"The quick brown fox jumps over the lazy dog"),
            0x22620404
        );
        // Software and hardware paths must agree at every length: both interleaved sizes,
        // the u64 loop, and the byte tail.
        let data: Vec<u8> = (0..9000u32).map(|i| (i * 7) as u8).collect();
        for n in (0..data.len()).step_by(7) {
            assert_eq!(crc32c_sw(0, &data[..n]), crc32c(&data[..n]), "len {n}");
        }
        assert_eq!(crc32c_with(crc32c(&data[..37]), &data[37..]), crc32c(&data));
    }

    #[test]
    fn mblock_round_trips_and_detects_damage() {
        for class in [Class::Mutable, Class::Immutable] {
            let mut e = vec![Entry::default(); class.k() as usize];
            e[3] = Entry {
                addr: 0x1234_5678_9abc,
                version: 42,
                ballot: 7,
                data_crc: 0xdead_beef,
                epoch: 9,
                state: State::Live,
                flags: 1,
            };
            let mut b = vec![0u8; MBLOCK];
            let h = Header {
                mblock_id: 11,
                generation: 5,
                class,
                live: 1,
            };
            put_mblock(&mut b, h, &e);

            let got = get_header(&b).unwrap();
            assert_eq!(got.mblock_id, 11);
            assert_eq!(got.generation, 5);
            assert_eq!(got.class, class);
            assert_eq!(got.live, 1);
            let mut want = e[3];
            if class == Class::Immutable {
                want.data_crc = 0; // huge entries have no field to hold it
            }
            assert_eq!(get_entry(&b, class, 3), want);
            assert_eq!(get_entry(&b, class, 4), Entry::default());

            // Any single flipped bit is caught.
            b[MBLOCK - 1] ^= 1;
            assert!(get_header(&b).is_none());
            b[MBLOCK - 1] ^= 1;
            b[MB_HDR] ^= 0x80;
            assert!(get_header(&b).is_none());
        }
    }

    fn test_config() -> crate::config::Config {
        crate::config::Config::parse(
            "node id=1 zone=1 store=/dev/x size=68719476736
             universe 1 fabric_device_id=9
               group 1 2 3
               extent id=1 base=0 pages=5000 kind=occ zone=1
               extent id=2 base=8192 pages=3 kind=immutable_4m zone=1
             device 1 extents=1
             device 2 extents=2",
        )
        .unwrap()
    }

    #[test]
    fn geometry_round_trips() {
        let cfg = test_config();
        let g = Geometry::plan(64 << 30, &cfg).unwrap();
        assert!(g.slots(Class::Mutable) >= 5000 && g.slots(Class::Immutable) >= 3);
        assert_eq!(
            g.slots(Class::Mutable),
            g.mblocks(Class::Mutable) * K_MUTABLE as u64
        );
        assert_eq!(g.extents(Class::Immutable)[0].data % HUGE_PAGE, 0);

        let mut b = vec![0u8; MBLOCK];
        g.encode(&mut b);
        assert_eq!(u16(&b[4..]), FMT_VER);
        assert_eq!(Geometry::decode(&b).unwrap(), g);
        b[100] ^= 1;
        assert!(Geometry::decode(&b).is_none());

        assert!(Geometry::plan(1 << 20, &cfg).is_err());
    }

    /// Every byte range a layout hands out, for overlap checks without enumerating slots.
    fn ranges(g: &Geometry) -> Vec<(u64, u64)> {
        let mut v = vec![(0, SB_REGION)];
        for class in CLASSES {
            let mut first = 0;
            for e in g.extents(class) {
                v.push((e.meta, e.meta + 2 * e.mblocks * MBLOCK as u64));
                let slots = e.mblocks * class.k() as u64;
                v.push((e.data, e.data + slots * class.bytes()));
                if e.mblocks > 0 {
                    let last = (first + e.mblocks - 1) as u32;
                    assert_eq!(g.mblock_off(class, first as u32, 0), e.meta);
                    assert_eq!(
                        g.mblock_off(class, last, 1),
                        v[v.len() - 2].1 - MBLOCK as u64
                    );
                    assert_eq!(g.slot_off(class, (first * class.k() as u64) as u32), e.data);
                    assert_eq!(g.ext_end(class, first), first + e.mblocks);
                }
                first += e.mblocks;
            }
        }
        v.retain(|(lo, hi)| lo < hi);
        v
    }

    #[test]
    fn growth_appends_without_moving_anything() {
        let cfg = test_config();
        let g0 = Geometry::plan(64 << 30, &cfg).unwrap();

        let mut g = g0;
        g.append(Class::Mutable, 40).unwrap();
        g.append(Class::Immutable, 2).unwrap();
        g.append(Class::Mutable, 7).unwrap();

        // Nothing already placed moved.
        assert_eq!(g.extents(Class::Mutable)[0], g0.extents(Class::Mutable)[0]);
        assert_eq!(g.extents(Class::Immutable)[0], g0.extents(Class::Immutable)[0]);
        assert!(g.alloc_end() > g0.alloc_end());

        // Ids simply continue, and every range is disjoint and on the device.
        assert_eq!(g.mblocks(Class::Mutable), g0.mblocks(Class::Mutable) + 47);
        assert_eq!(g.mblocks(Class::Immutable), g0.mblocks(Class::Immutable) + 2);
        let v = ranges(&g);
        for (i, a) in v.iter().enumerate() {
            assert!(a.1 <= g.alloc_end(), "{a:?} runs past the end");
            for b in &v[i + 1..] {
                assert!(a.1 <= b.0 || b.1 <= a.0, "{a:?} overlaps {b:?}");
            }
        }

        let mut b = vec![0u8; MBLOCK];
        g.encode(&mut b);
        assert_eq!(u16(&b[4..]), FMT_VER);
        assert_eq!(Geometry::decode(&b).unwrap(), g);

        // Patching a live superblock leaves the consensus record alone.
        let c = Consensus {
            terms: vec![(GroupId::new(4, 7), 9)],
            ..Consensus::default()
        };
        c.patch(&mut b);
        g.append(Class::Immutable, 1).unwrap();
        g.patch(&mut b);
        assert_eq!(Geometry::decode(&b).unwrap(), g);
        assert_eq!(Consensus::decode(&b).terms, vec![(GroupId::new(4, 7), 9)]);
    }

    /// The layout claims the whole store, and what it could not claim is the cache's.
    #[test]
    fn the_layout_claims_the_whole_store() {
        let cfg = test_config();
        const SIZE: u64 = 64 << 30;
        let g0 = Geometry::plan(SIZE, &cfg).unwrap();

        // What is left over is alignment slack and the remainder of one run, never idle media.
        let per = 2 * MBLOCK as u64 + K_IMMUTABLE as u64 * HUGE_PAGE;
        assert!(g0.alloc_end() <= SIZE);
        assert!(SIZE - g0.alloc_end() < per);

        let (base, len) = g0.tail(SIZE);
        assert_eq!(base % CHUNK_BYTES, 0);
        assert!(base >= g0.alloc_end());
        assert_eq!(len % CHUNK_BYTES, 0);
        assert!(len < per);
        assert_eq!(g0.tail_chunks(SIZE), len / CHUNK_BYTES);

        // A bigger store is more slots a growing zone can reach without a restart, and the
        // 4 MiB class is where that room goes: a small slot costs DRAM to index.
        let g1 = Geometry::plan(SIZE * 2, &cfg).unwrap();
        assert!(g1.slots(Class::Immutable) > g0.slots(Class::Immutable));
        assert_eq!(g1.slots(Class::Mutable), g0.slots(Class::Mutable));
        assert!(g1.alloc_end() <= SIZE * 2);

        // No room past the slabs means no tail, reported rather than wrapped.
        assert_eq!(g0.tail(g0.alloc_end()).1, 0);
        assert_eq!(g0.tail_chunks(0), 0);
    }

    /// A growth appends, so what [`claim`] sized against a layout placed from scratch does
    /// not all fit. A store whose config has since shrunk is the clearest case: the slots
    /// the deleted volume was given are still placed, and a fresh layout cannot see them.
    #[test]
    fn growth_claims_only_what_appending_fits() {
        // The config the store was formatted for, then the smaller one it now carries.
        let before = crate::config::Config::parse(
            "node id=1 zone=1 store=/dev/x size=68719476736
             universe 1 fabric_device_id=9
               group 1 2 3
               extent id=1 base=0 pages=400000 kind=occ zone=1
             device 1 extents=1",
        )
        .unwrap();
        let cfg = test_config();
        const FIRST: u64 = 2 << 30;
        const SECOND: u64 = 12 << 30;

        let g = Geometry::plan(FIRST, &before).unwrap();
        let want = claim(SECOND, &cfg);
        let have: [u64; 2] = CLASSES.map(|c| g.mblocks(c));
        let raw: [u64; 2] = std::array::from_fn(|i| want[i].saturating_sub(have[i]));

        let appended = |add: [u64; 2]| {
            let mut t = g;
            for (i, class) in CLASSES.into_iter().enumerate() {
                if add[i] > 0 {
                    t.append(class, add[i]).unwrap();
                }
            }
            t
        };

        // The untrimmed claim is what a fresh layout would hold, not what this one can.
        assert_eq!(raw[0], 0, "the 4 KiB class already has more than it needs");
        assert!(Geometry::place(want).alloc_end() <= SECOND);
        assert!(appended(raw).alloc_end() > SECOND);

        // Trimmed it fits, and it still hands the 4 MiB class the room it did find.
        let add = fitted(&g, raw, SECOND);
        assert!(add[1] < raw[1]);
        let fit = appended(add);
        assert!(fit.alloc_end() <= SECOND);
        fit.check(SECOND).unwrap();
        assert!(fit.slots(Class::Immutable) > g.slots(Class::Immutable));

        // A store already big enough asks for nothing and is left alone.
        assert_eq!(fitted(&g, [0, 0], SECOND), [0, 0]);
    }

    #[test]
    fn growth_is_bounded() {
        let mut g = Geometry::plan(1 << 40, &test_config()).unwrap();
        for _ in 1..MAX_EXT {
            g.append(Class::Immutable, 1).unwrap();
        }
        assert!(g.append(Class::Immutable, 1).is_err());

        // A class whose slots would outgrow a 32-bit id is refused, not truncated.
        let mut g = Geometry::plan(1 << 40, &test_config()).unwrap();
        g.append(Class::Mutable, u32::MAX as u64 / K_MUTABLE as u64 + 1)
            .unwrap();
        assert!(g.check(u64::MAX).is_err());
    }

    /// The store file is created, held at the configured size, and never shrunk.
    #[test]
    fn the_store_is_created_and_only_ever_grows() {
        let dir = std::env::temp_dir().join(format!("racer-size-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        // A path two levels down, so the parent really has to be made.
        let path = dir.join("d").join("store.img");

        let sized = |bytes: u64| {
            let mut c = test_config();
            c.node.store = path.clone();
            c.node.set_store_bytes(bytes);
            c
        };

        // Nothing there: directory and file are both created, at the full size.
        size_if_needed(&path, &sized(8 << 20)).unwrap();
        assert_eq!(std::fs::metadata(&path).unwrap().len(), 8 << 20);
        // Mode 0600, since it holds guest data.
        let mode = std::os::unix::fs::MetadataExt::mode(&std::fs::metadata(&path).unwrap());
        assert_eq!(mode & 0o777, 0o600);

        // Asking for the size it already is touches nothing.
        size_if_needed(&path, &sized(8 << 20)).unwrap();
        assert_eq!(std::fs::metadata(&path).unwrap().len(), 8 << 20);

        // A raised size is taken, and what was already written stays put.
        let f = open_direct(&path, true).unwrap();
        let mut buf = Aligned::new(MBLOCK);
        buf.as_mut().fill(0xa5);
        write_at(&f, buf.as_ref(), 4 << 20).unwrap();
        drop(f);

        size_if_needed(&path, &sized(32 << 20)).unwrap();
        assert_eq!(std::fs::metadata(&path).unwrap().len(), 32 << 20);
        let f = open_direct(&path, false).unwrap();
        buf.as_mut().fill(0);
        read_at(&f, buf.as_mut(), 4 << 20).unwrap();
        assert!(buf.as_ref().iter().all(|&b| b == 0xa5), "page survived");
        drop(f);

        // A lowered one is refused naming both sizes, and costs the file nothing.
        let e = size_if_needed(&path, &sized(16 << 20)).expect_err("a store cannot shrink");
        let msg = e.to_string();
        assert!(msg.contains("33554432"), "the size it is: {msg}");
        assert!(msg.contains("16777216"), "the size it was asked for: {msg}");
        assert_eq!(std::fs::metadata(&path).unwrap().len(), 32 << 20);

        std::fs::remove_dir_all(&dir).unwrap();
    }
}
