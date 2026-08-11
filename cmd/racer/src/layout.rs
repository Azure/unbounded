//! On-disk formats: pure codec and arithmetic, no state or runtime. The exception is
//! the format/read-back path at the bottom, which runs on the control thread with
//! plain syscalls.
//!
//! ```text
//! +-----------+--------+-----------------------------+-------------------------+-------------+
//! | Superblk  | zeroes |      Metadata region        |       Data region       |    Cache    |
//! | x4 copies |  4 MiB | small A|B | huge A|B        | 4 KiB slab | 4 MiB slab | 4k  |  4m   |
//! +-----------+--------+-----------------------------+-------------------------+-------------+
//!                       <---------- authoritative, mblock-backed -------------> <-no metadata->
//! ```
//!
//! The 4 MiB of zeroes belongs to no region: it is the source a read of a never-written
//! page DMAs from.

use std::io;
#[cfg(not(feature = "sim"))]
use std::os::fd::AsRawFd;
use std::path::Path;
use std::sync::Arc;

use crate::runtime::Limiter;

pub(crate) const MBLOCK: usize = 4096;
const MB_HDR: usize = 64;
/// Entry widths. Only the 4 KiB entry carries `data_crc`; 4 MiB pages are not
/// checksummed.
const ENTRY_SMALL: usize = 36;
const ENTRY_HUGE: usize = 32;
/// Slots per mblock. Both are exact fits: 64 + 112*36 = 64 + 126*32 = 4096.
const K_SMALL: u32 = 112;
const K_HUGE: u32 = 126;

pub(crate) const SMALL_PAGE: u64 = 4096;
pub(crate) const HUGE_PAGE: u64 = 4 << 20;

const SB_MAGIC: u32 = 0x5243_5342; // "RCSB"
const MB_MAGIC: u32 = 0x524d_4232; // "RMB2"
/// The layout the format shipped with: one run of mblocks per class.
const FMT_VER: u16 = 1;
/// A layout `grow` has appended to. A build that predates growth must not open one, so
/// it gets its own version rather than a flag: that build would read extent 0's block
/// count as the whole class and put copy B of every block at the wrong offset.
const FMT_VER_EXT: u16 = 2;
const SB_COPIES: u64 = 4;
const SB_REGION: u64 = SB_COPIES * MBLOCK as u64;
const ZERO_BYTES: u64 = HUGE_PAGE;
/// Superblock tail reserved for the consensus record (`promised_term` and the seal
/// table); the geometry words and the growth table must not reach into it.
const SB_RESERVED: usize = 3072;

/// Both classes in the order their ids are tagged with on disk.
const CLASSES: [Class; 2] = [Class::Small, Class::Huge];

fn ver_ok(v: u16) -> bool {
    v == FMT_VER || v == FMT_VER_EXT
}

/// Which slab a page lives in. This, not the extent's type, decides whether the page
/// is checksummed.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub(crate) enum Class {
    Small,
    Huge,
}

impl Class {
    pub(crate) fn bytes(self) -> u64 {
        match self {
            Class::Small => SMALL_PAGE,
            Class::Huge => HUGE_PAGE,
        }
    }

    fn entry_bytes(self) -> usize {
        match self {
            Class::Small => ENTRY_SMALL,
            Class::Huge => ENTRY_HUGE,
        }
    }

    /// Slots per mblock.
    pub(crate) fn k(self) -> u32 {
        match self {
            Class::Small => K_SMALL,
            Class::Huge => K_HUGE,
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug, Default)]
#[repr(u8)]
pub(crate) enum State {
    #[default]
    Empty = 0,
    Live = 1,
    /// Immutable page written and then trimmed. The entry survives so a reader can tell
    /// a trimmed page from a hole (CORFU).
    Tombstone = 2,
}

/// One mblock entry, decoded. Also the in-DRAM slot record: the mblock image is kept in
/// memory as the authoritative copy, so a metadata write is a full-block rewrite with no
/// read-modify-write.
#[derive(Clone, Copy, PartialEq, Eq, Hash, Debug, Default)]
pub(crate) struct Entry {
    /// The page in this slot. Zero means free, which is why volume id 0 is reserved.
    pub(crate) addr: u64,
    pub(crate) version: u64,
    /// CASPaxos accepted ballot (`paxos::Ballot`), in the low 32 bits.
    pub(crate) ballot: u64,
    /// CRC32C of the page bytes seeded with `(addr, version)`. Meaningful only when
    /// `state == Live` and the class is `Small`.
    pub(crate) data_crc: u32,
    /// Tombstone epoch, for the tombstone sweep.
    pub(crate) epoch: u32,
    pub(crate) state: State,
    pub(crate) flags: u8,
}

impl Entry {
    fn decode(class: Class, b: &[u8]) -> Entry {
        let state = match b[if class == Class::Small { 28 } else { 24 }] {
            1 => State::Live,
            2 => State::Tombstone,
            _ => State::Empty,
        };
        let (flags, epoch, crc) = match class {
            Class::Small => (b[29], u32(&b[32..]), u32(&b[24..])),
            Class::Huge => (b[25], u32(&b[28..]), 0),
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
            Class::Small => {
                b[24..28].copy_from_slice(&self.data_crc.to_le_bytes());
                b[28] = self.state as u8;
                b[29] = self.flags;
                b[30..32].fill(0);
                b[32..36].copy_from_slice(&self.epoch.to_le_bytes());
            }
            Class::Huge => {
                b[24] = self.state as u8;
                b[25] = self.flags;
                b[26..28].fill(0);
                b[28..32].copy_from_slice(&self.epoch.to_le_bytes());
            }
        }
    }
}

// ---------------------------------------------------------------------------- mblock

/// Header of a decoded mblock. `mblock_id` and the class fix which run of slots this
/// block describes, so a slot's index is positional and never stored.
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

/// Validate and decode an mblock header. `None` means this copy is unusable: wrong
/// magic, wrong format, or a failed CRC. The caller falls back to the other copy and
/// quarantines the block if both fail.
pub(crate) fn get_header(buf: &[u8]) -> Option<Header> {
    if buf.len() != MBLOCK
        || u32(&buf[0..]) != MB_MAGIC
        || u16(&buf[4..]) != FMT_VER
        || u32(&buf[CRC_OFF..]) != mblock_crc(buf)
    {
        return None;
    }
    let class = match buf[6] {
        0 => Class::Small,
        1 => Class::Huge,
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

/// One contiguous run of mblocks for a single class: copy A of its metadata, copy B a
/// whole run later, and the data slots those blocks name. Extent 0 is what `format`
/// laid down; `grow` appends the rest past the end of everything, which is what lets
/// capacity be added without moving a byte that is already there.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
struct Extent {
    meta: u64,
    data: u64,
    mblocks: u64,
}

/// Extents per class, extent 0 included. Each one at least doubles the class, so eight
/// is past any device that could be built.
const MAX_EXT: usize = 8;

/// Where every region starts and how big it is. Computed at format time from the config
/// and carried in the superblock; `grow` appends to it, and nothing else recomputes it
/// at runtime.
#[derive(Clone, Copy, Debug, PartialEq, Default)]
pub struct Geometry {
    pub(crate) zero_base: u64,
    pub(crate) cache_small: u64,
    pub(crate) cache_small_bytes: u64,
    pub(crate) cache_huge: u64,
    pub(crate) cache_huge_bytes: u64,
    /// First byte past everything the layout uses. `grow` places from here.
    total: u64,
    small: [Extent; MAX_EXT],
    huge: [Extent; MAX_EXT],
    n_small: u8,
    n_huge: u8,
}

/// Spare slots beyond the config's page count, for in-flight out-of-place writes and
/// tombstones awaiting an epoch advance. 5% plus a per-class `floor` that keeps tiny
/// configs workable; per class because 64 spare huge slots would be a quarter of a
/// gigabyte.
fn overprovision(pages: u64, floor: u64) -> u64 {
    if pages == 0 { 0 } else { pages + pages / 20 + floor }
}

fn align_up(x: u64, a: u64) -> u64 {
    x.div_ceil(a) * a
}

/// The geometry words: extent 0 of each class and the regions growth never moves.
const WORDS: usize = 14;
/// The growth table, between the geometry words and the consensus record. A flat list
/// of the extents `grow` appended, tagged with their class.
const GT_MAGIC: u32 = 0x5247_5854; // "RGXT"
const GT_OFF: usize = 16 + WORDS * 8;
const GT_HDR: usize = 16;
const EXT_BYTES: usize = 32;
const GT_ROWS: usize = CLASSES.len() * (MAX_EXT - 1);
const _: () = assert!(GT_OFF + GT_HDR + GT_ROWS * EXT_BYTES <= MBLOCK - SB_RESERVED);

impl Geometry {
    /// Size every region from the config. Fails if the device is too small.
    fn plan(dev_bytes: u64, cfg: &crate::config::Config) -> io::Result<Geometry> {
        let mut g = Geometry::default();
        let want = wanted(cfg);

        let mut at = SB_REGION;
        g.zero_base = at;
        at += ZERO_BYTES;
        // Both classes' metadata, then both classes' data, so the two metadata regions
        // sit together at the head of the device.
        let meta = [at, at + want[0] * 2 * MBLOCK as u64];
        at = meta[1] + want[1] * 2 * MBLOCK as u64;
        for (i, class) in CLASSES.into_iter().enumerate() {
            at = align_up(at, class.bytes());
            let e = Extent { meta: meta[i], data: at, mblocks: want[i] };
            at += e.mblocks * class.k() as u64 * class.bytes();
            g.push(class, e).expect("first extent of an empty geometry");
        }
        g.cache_small = at;
        g.cache_small_bytes = cfg.node.cache_bytes_4k / SMALL_PAGE * SMALL_PAGE;
        at += g.cache_small_bytes;
        at = align_up(at, HUGE_PAGE);
        g.cache_huge = at;
        g.cache_huge_bytes = cfg.node.cache_bytes_4m / HUGE_PAGE * HUGE_PAGE;
        at += g.cache_huge_bytes;
        g.total = at;

        g.check(dev_bytes)?;
        Ok(g)
    }

    /// The runs of mblocks this class is made of, in id order.
    fn extents(&self, class: Class) -> &[Extent] {
        match class {
            Class::Small => &self.small[..self.n_small as usize],
            Class::Huge => &self.huge[..self.n_huge as usize],
        }
    }

    /// Append a run. `None` once a class has been grown `MAX_EXT` times.
    fn push(&mut self, class: Class, e: Extent) -> Option<()> {
        let (a, n) = match class {
            Class::Small => (&mut self.small, &mut self.n_small),
            Class::Huge => (&mut self.huge, &mut self.n_huge),
        };
        *a.get_mut(*n as usize)? = e;
        *n += 1;
        Some(())
    }

    /// Place a run of `n` more mblocks past the end of everything, taking the layout out
    /// to its new end. The only code that decides where a grown extent goes, and it
    /// reads nothing but the layout itself: a growth cut short by a crash is placed at
    /// the same offsets and rewritten on the next boot.
    fn append(&mut self, class: Class, n: u64) -> io::Result<()> {
        let meta = self.total;
        let data = align_up(meta + n * 2 * MBLOCK as u64, class.bytes());
        self.total = data + n * class.k() as u64 * class.bytes();
        self.push(class, Extent { meta, data, mblocks: n })
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!("device has already been grown {} times", MAX_EXT - 1),
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

    /// The first mblock id past the run holding `id`. A batched read of the metadata
    /// region stops here: the next run's blocks are elsewhere on the device.
    pub(crate) fn ext_end(&self, class: Class, id: u64) -> u64 {
        self.ext_of(class, id).map_or(id, |(e, first)| first + e.mblocks)
    }

    /// Byte offset of a data slot.
    pub(crate) fn slot_off(&self, class: Class, slot: u32) -> u64 {
        let k = class.k() as u64;
        let (e, first) = self.ext_of(class, slot as u64 / k).expect("slot is within the geometry");
        e.data + (slot as u64 - first * k) * class.bytes()
    }

    /// Slots in a class's cache region. The region is statically carved, so this is a
    /// division and not an accounting question: the cache has no free list. It is also
    /// the one region growth leaves alone.
    pub(crate) fn cache_slots(&self, class: Class) -> u64 {
        match class {
            Class::Small => self.cache_small_bytes,
            Class::Huge => self.cache_huge_bytes,
        }
        .checked_div(class.bytes())
        .unwrap_or(0)
    }

    /// Byte offset of a cache slot. A space separate from `slot_off`: cache pressure
    /// can never reach the allocator's watermarks, and the allocator can never reclaim
    /// cache space.
    pub(crate) fn cache_off(&self, class: Class, slot: u32) -> u64 {
        let base = match class {
            Class::Small => self.cache_small,
            Class::Huge => self.cache_huge,
        };
        base + slot as u64 * class.bytes()
    }

    /// Byte offset of one copy of an mblock. Copies A and B sit a whole run apart
    /// rather than adjacent, so one bad neighbourhood of the device cannot take both
    /// copies of a block.
    pub(crate) fn mblock_off(&self, class: Class, id: u32, copy: u8) -> u64 {
        let (e, first) = self.ext_of(class, id as u64).expect("mblock id is within the geometry");
        e.meta + (copy as u64 * e.mblocks + (id as u64 - first)) * MBLOCK as u64
    }

    /// Whether this layout has been grown. A grown device is written at a format version
    /// an older build refuses, because that build would read extent 0's block count as
    /// the whole class and address copy B of every block at the wrong offset.
    fn grown(&self) -> bool {
        self.n_small > 1 || self.n_huge > 1
    }

    /// The two limits a layout has to stay inside, checked wherever one is built.
    fn check(&self, dev_bytes: u64) -> io::Result<()> {
        if self.total > dev_bytes {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("config needs {} B, device has {dev_bytes} B", self.total),
            ));
        }
        // Slot and mblock ids are u32 everywhere: on the wire, in the free lists, and in
        // the mblock header. Reachable on a large device only after several growths.
        for class in CLASSES {
            if self.slots(class) > u32::MAX as u64 {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!("{} slots is past what a 32-bit slot id can name", self.slots(class)),
                ));
            }
        }
        Ok(())
    }

    /// Write the geometry into a superblock image the caller has already read, leaving
    /// the consensus record alone and refreshing the block CRC. `grow` rewrites a live
    /// superblock this way; `format` has nothing to preserve and uses `encode`.
    fn patch(&self, b: &mut [u8]) {
        b[..CS_OFF].fill(0);
        b[0..4].copy_from_slice(&SB_MAGIC.to_le_bytes());
        b[4..6].copy_from_slice(&if self.grown() { FMT_VER_EXT } else { FMT_VER }.to_le_bytes());
        b[6..8].copy_from_slice(&(K_SMALL as u16).to_le_bytes());
        b[8..10].copy_from_slice(&(K_HUGE as u16).to_le_bytes());
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
            || u16(&b[6..]) as u32 != K_SMALL
            || u16(&b[8..]) as u32 != K_HUGE
            || u32(&b[MBLOCK - 4..]) != crc32c(&b[..MBLOCK - 4])
        {
            return None;
        }
        let w: [u64; WORDS] = std::array::from_fn(|i| u64f(&b[16 + i * 8..]));
        // Words 7 and 8 are extent 0's slot counts, which are its block count times the
        // class's k. Redundant, and kept because the first version of the format wrote
        // them: checked here rather than trusted.
        if w[7] != w[3] * K_SMALL as u64 || w[8] != w[4] * K_HUGE as u64 {
            return None;
        }
        let mut g = Geometry {
            zero_base: w[0],
            cache_small: w[9],
            cache_small_bytes: w[10],
            cache_huge: w[11],
            cache_huge_bytes: w[12],
            total: w[13],
            ..Geometry::default()
        };
        g.push(Class::Small, Extent { meta: w[1], data: w[5], mblocks: w[3] })?;
        g.push(Class::Huge, Extent { meta: w[2], data: w[6], mblocks: w[4] })?;
        g.load_growth(b)?;
        Some(g)
    }

    /// Extent 0 of each class, plus the regions growth never touches. This is the layout
    /// the first version of the format had, kept word for word so a device written by it
    /// still opens.
    fn words(&self) -> [u64; WORDS] {
        let (s, h) = (self.small[0], self.huge[0]);
        [
            self.zero_base,
            s.meta,
            h.meta,
            s.mblocks,
            h.mblocks,
            s.data,
            h.data,
            s.mblocks * K_SMALL as u64,
            h.mblocks * K_HUGE as u64,
            self.cache_small,
            self.cache_small_bytes,
            self.cache_huge,
            self.cache_huge_bytes,
            self.total,
        ]
    }

    /// The extents `grow` appended, in id order within each class. Absent on a device
    /// that has never grown, and on one formatted before this table existed.
    fn save_growth(&self, b: &mut [u8]) {
        if !self.grown() {
            return;
        }
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
        b[GT_OFF + 4..GT_OFF + 6].copy_from_slice(&FMT_VER_EXT.to_le_bytes());
        b[GT_OFF + 6..GT_OFF + 8].copy_from_slice(&n.to_le_bytes());
    }

    fn load_growth(&mut self, b: &[u8]) -> Option<()> {
        if u32(&b[GT_OFF..]) != GT_MAGIC {
            return Some(());
        }
        let n = u16(&b[GT_OFF + 6..]) as usize;
        if u16(&b[GT_OFF + 4..]) != FMT_VER_EXT || n > GT_ROWS {
            return None;
        }
        for i in 0..n {
            let at = GT_OFF + GT_HDR + i * EXT_BYTES;
            let class = CLASSES[(u32(&b[at..]) as usize).min(CLASSES.len() - 1)];
            self.push(class, Extent {
                meta: u64f(&b[at + 8..]),
                data: u64f(&b[at + 16..]),
                mblocks: u64f(&b[at + 24..]),
            })?;
        }
        Some(())
    }
}

/// Mblocks each class needs for `cfg`, the same rounding `plan` and `grow` both work
/// from so that a config which fits stays fitting.
fn wanted(cfg: &crate::config::Config) -> [u64; 2] {
    [
        overprovision(cfg.small_pages(), 64).div_ceil(K_SMALL as u64),
        overprovision(cfg.huge_pages(), 4).div_ceil(K_HUGE as u64),
    ]
}

// -------------------------------------------------------------- consensus side state

/// One row of the seal table: a shard this node's group has frozen as a migration
/// source. `extent` is the extent's index in its volume's list.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct Seal {
    pub(crate) volume: u32,
    pub(crate) extent: u32,
    pub(crate) term: u32,
}

/// The two things consensus keeps that are not page registers: `promised_term` per
/// group, and the seal table. Both live in the superblock rather than an mblock
/// because neither is addressed by page.
///
/// Losing a seal is the one metadata loss that is not self-healing — it is a shard that
/// could then be written in two zones — so it gets the superblock's fourfold redundancy.
#[derive(Clone, Debug, Default, PartialEq)]
pub(crate) struct Consensus {
    /// `(group, promised_term)`, sorted by group.
    pub(crate) terms: Vec<(u32, u32)>,
    pub(crate) seals: Vec<Seal>,
}

const CS_MAGIC: u32 = 0x5250_5831; // "RPX1"
const CS_OFF: usize = 1024;
const CS_HDR: usize = 16;
const TERM_BYTES: usize = 8;
const SEAL_BYTES: usize = 16;
/// Bounded by what fits beside the geometry in one 4 KiB block, well above the groups
/// a node joins and the shards it can be migrating at once.
pub(crate) const MAX_TERMS: usize = 128;
pub(crate) const MAX_SEALS: usize = 96;
const _: () = assert!(CS_OFF + CS_HDR + MAX_TERMS * TERM_BYTES + MAX_SEALS * SEAL_BYTES <= MBLOCK - 4);
const _: () = assert!(CS_OFF == MBLOCK - SB_RESERVED);

impl Consensus {
    /// Patch this state into a superblock image the caller has already read, leaving the
    /// geometry words alone and refreshing the block CRC.
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
            b[at..at + 4].copy_from_slice(&g.to_le_bytes());
            b[at + 4..at + 8].copy_from_slice(&t.to_le_bytes());
            at += TERM_BYTES;
        }
        for s in &self.seals {
            b[at..at + 4].copy_from_slice(&s.volume.to_le_bytes());
            b[at + 4..at + 8].copy_from_slice(&s.extent.to_le_bytes());
            b[at + 12..at + 16].copy_from_slice(&s.term.to_le_bytes());
            at += SEAL_BYTES;
        }
        let crc = crc32c(&b[..MBLOCK - 4]);
        b[MBLOCK - 4..].copy_from_slice(&crc.to_le_bytes());
    }

    /// Decode from a superblock image whose CRC the caller has already checked. A block
    /// written before this record existed has no magic and reads as empty, the correct
    /// state for a device that has never run consensus.
    fn decode(b: &[u8]) -> Consensus {
        let mut c = Consensus::default();
        if b.len() != MBLOCK || u32(&b[CS_OFF..]) != CS_MAGIC || u16(&b[CS_OFF + 4..]) != FMT_VER {
            return c;
        }
        let nt = (u16(&b[CS_OFF + 6..]) as usize).min(MAX_TERMS);
        let ns = (u16(&b[CS_OFF + 8..]) as usize).min(MAX_SEALS);
        let mut at = CS_OFF + CS_HDR;
        for _ in 0..nt {
            c.terms.push((u32(&b[at..]), u32(&b[at + 4..])));
            at += TERM_BYTES;
        }
        for _ in 0..ns {
            c.seals.push(Seal {
                volume: u32(&b[at..]),
                extent: u32(&b[at + 4..]),
                term: u32(&b[at + 12..]),
            });
            at += SEAL_BYTES;
        }
        c
    }
}

/// Byte offset of superblock copy `i`. Consensus state is rewritten through every
/// copy; it is off every hot path.
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
        "no valid superblock; device is not formatted",
    ))
}

// ------------------------------------------------------------------------------- CRC

/// CRC32C (Castagnoli). `seed` is a previous result, so
/// `crc32c_with(crc32c(a), b) == crc32c(a ++ b)`.
fn crc32c_with(seed: u32, data: &[u8]) -> u32 {
    #[cfg(target_arch = "x86_64")]
    if std::is_x86_feature_detected!("sse4.2") {
        // Safety: guarded by the feature check above.
        return unsafe { crc32c_hw(seed, data) };
    }
    crc32c_sw(seed, data)
}

pub fn crc32c(data: &[u8]) -> u32 {
    crc32c_with(0, data)
}

/// The page checksum, seeded with the address and version so that a misdirected read,
/// or a page left over from a lost metadata write, fails the check even though its
/// bytes are internally consistent.
pub fn page_crc(addr: u64, version: u64, page: &[u8]) -> u32 {
    let mut seed = [0u8; 16];
    seed[0..8].copy_from_slice(&addr.to_le_bytes());
    seed[8..16].copy_from_slice(&version.to_le_bytes());
    crc32c_with(crc32c(&seed), page)
}

// The CRC32 instruction has three cycles of latency and issues one per cycle, so a
// single accumulator runs the pipeline a third full. Three interleaved chains fill it;
// they are folded back into one by advancing the earlier chains over the bytes the
// later ones covered. That advance is a fixed linear map over GF(2), so it is a
// constant matrix per chunk length.
#[cfg(target_arch = "x86_64")]
const LONG: usize = 1024;
#[cfg(target_arch = "x86_64")]
const SHORT: usize = 256;
#[cfg(target_arch = "x86_64")]
static LONG_OP: Shift = shift_table(&zeros(LONG));
#[cfg(target_arch = "x86_64")]
static SHORT_OP: Shift = shift_table(&zeros(SHORT));

#[cfg(target_arch = "x86_64")]
#[target_feature(enable = "sse4.2")]
unsafe fn crc32c_hw(seed: u32, data: &[u8]) -> u32 {
    use std::arch::x86_64::{_mm_crc32_u8, _mm_crc32_u64};
    let mut crc = !seed as u64;
    let mut p = data;
    unsafe {
        while p.len() >= 3 * LONG {
            crc = triple(crc, p, LONG, &LONG_OP);
            p = &p[3 * LONG..];
        }
        while p.len() >= 3 * SHORT {
            crc = triple(crc, p, SHORT, &SHORT_OP);
            p = &p[3 * SHORT..];
        }
    }
    let mut it = p.chunks_exact(8);
    for c in &mut it {
        crc = _mm_crc32_u64(crc, u64::from_le_bytes(c.try_into().unwrap()));
    }
    let mut crc = crc as u32;
    for &b in it.remainder() {
        crc = _mm_crc32_u8(crc, b);
    }
    !crc
}

/// One `3 * n` byte block, `n` a multiple of eight, folded back to a single CRC.
#[cfg(target_arch = "x86_64")]
#[target_feature(enable = "sse4.2")]
unsafe fn triple(seed: u64, block: &[u8], n: usize, op: &Shift) -> u64 {
    use std::arch::x86_64::_mm_crc32_u64;
    let (mut a, mut b, mut c) = (seed, 0u64, 0u64);
    let p = block.as_ptr();
    let mut i = 0;
    // Safety: the caller has checked `block.len() >= 3 * n`.
    unsafe {
        while i < n {
            a = _mm_crc32_u64(a, p.add(i).cast::<u64>().read_unaligned().to_le());
            b = _mm_crc32_u64(b, p.add(n + i).cast::<u64>().read_unaligned().to_le());
            c = _mm_crc32_u64(c, p.add(2 * n + i).cast::<u64>().read_unaligned().to_le());
            i += 8;
        }
    }
    let ab = shift(op, a as u32) ^ b as u32;
    (shift(op, ab) ^ c as u32) as u64
}

/// The bit-reflected CRC32C (Castagnoli) polynomial.
const POLY: u32 = 0x82f6_3b78;

/// Apply a GF(2) matrix, each column a 32-bit vector, to a CRC.
#[cfg(target_arch = "x86_64")]
const fn apply(m: &[u32; 32], mut v: u32) -> u32 {
    let mut sum = 0;
    let mut i = 0;
    while v != 0 {
        if v & 1 != 0 {
            sum ^= m[i];
        }
        v >>= 1;
        i += 1;
    }
    sum
}

/// A matrix flattened to one table per input byte, so a fold is four loads and three
/// XORs rather than a data-dependent walk over 32 columns.
#[cfg(target_arch = "x86_64")]
type Shift = [[u32; 256]; 4];

#[cfg(target_arch = "x86_64")]
const fn shift_table(m: &[u32; 32]) -> Shift {
    let mut t = [[0u32; 256]; 4];
    let mut j = 0;
    while j < 4 {
        let mut b = 0;
        while b < 256 {
            t[j][b] = apply(m, (b as u32) << (8 * j));
            b += 1;
        }
        j += 1;
    }
    t
}

#[cfg(target_arch = "x86_64")]
#[inline]
fn shift(t: &Shift, v: u32) -> u32 {
    t[0][(v & 0xff) as usize]
        ^ t[1][(v >> 8 & 0xff) as usize]
        ^ t[2][(v >> 16 & 0xff) as usize]
        ^ t[3][(v >> 24) as usize]
}

/// `a` after `b`, as one matrix.
#[cfg(target_arch = "x86_64")]
const fn compose(a: &[u32; 32], b: &[u32; 32]) -> [u32; 32] {
    let mut out = [0u32; 32];
    let mut i = 0;
    while i < 32 {
        out[i] = apply(a, b[i]);
        i += 1;
    }
    out
}

/// The operator that advances a CRC over `len` zero bytes.
#[cfg(target_arch = "x86_64")]
const fn zeros(len: usize) -> [u32; 32] {
    // One zero bit: shift right, and reduce by the polynomial when a bit falls off.
    let mut bit = [0u32; 32];
    bit[0] = POLY;
    let mut i = 1;
    while i < 32 {
        bit[i] = 1 << (i - 1);
        i += 1;
    }
    // Square three times to reach one zero byte, then raise that to the `len`th.
    let mut step = compose(&bit, &bit);
    step = compose(&step, &step);
    step = compose(&step, &step);
    let mut out = [0u32; 32];
    let mut i = 0;
    while i < 32 {
        out[i] = 1 << i;
        i += 1;
    }
    let mut n = len;
    while n > 0 {
        if n & 1 != 0 {
            out = compose(&step, &out);
        }
        step = compose(&step, &step);
        n >>= 1;
    }
    out
}

const TABLE: [u32; 256] = {
    let mut t = [0u32; 256];
    let mut i = 0;
    while i < 256 {
        let mut c = i as u32;
        let mut k = 0;
        while k < 8 {
            c = if c & 1 != 0 { 0x82f6_3b78 ^ (c >> 1) } else { c >> 1 };
            k += 1;
        }
        t[i] = c;
        i += 1;
    }
    t
};

fn crc32c_sw(seed: u32, data: &[u8]) -> u32 {
    let mut c = !seed;
    for &b in data {
        c = TABLE[((c ^ b as u32) & 0xff) as usize] ^ (c >> 8);
    }
    !c
}

// ----------------------------------------------------------------------------- format

/// Lay a device out for `cfg`: superblocks, the zero region, and an empty mblock for
/// every slot. Destroys whatever was there.
pub fn format(path: &Path, cfg: &crate::config::Config) -> io::Result<Geometry> {
    // Writing every mblock is the heaviest run of IO the device ever sees, and it is
    // metered the same as anything else.
    let f = open_direct(path, true)?.meter(Arc::new(Limiter::new(
        cfg.node.device_max_iops,
        cfg.node.device_max_bytes_per_sec,
    )));
    let g = Geometry::plan(device_bytes(&f)?, cfg)?;

    // One aligned staging buffer, reused. 4 MiB is both the zero-region unit and a
    // 1024-mblock batch.
    let mut buf = Aligned::new(HUGE_PAGE as usize);

    buf.as_mut().fill(0);
    for off in (0..ZERO_BYTES).step_by(HUGE_PAGE as usize) {
        write_at(&f, buf.as_ref(), g.zero_base + off)?;
    }

    for class in CLASSES {
        write_empty(&f, &g, class, 0, g.mblocks(class), &mut buf)?;
    }

    // Superblocks last, and only once every block they name is on the device: until
    // they land the device is not formatted.
    sync(&f)?;
    g.encode(&mut buf.as_mut()[..MBLOCK]);
    for i in 0..SB_COPIES {
        write_at(&f, &buf.as_ref()[..MBLOCK], sb_off(i))?;
    }
    sync(&f)?;
    Ok(g)
}

/// Write empty mblocks for `n` blocks of `class` from `first`, both copies, batching a
/// whole huge page at a time through the caller's staging buffer.
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
                // Both copies start equal; the A/B resolver breaks the tie toward A,
                // and the first real write lands on B at generation 1.
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

/// Format a blank device, or leave an existing valid layout untouched.
pub fn format_if_needed(path: &Path, cfg: &crate::config::Config) -> io::Result<()> {
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

/// Give `cfg` the slots the device was not formatted for, without disturbing what is
/// already on it. Everything new goes past the end of the layout, so no existing offset
/// moves and no existing byte is read or rewritten; the pages already stored survive
/// untouched, which is the whole point.
///
/// Called at startup, before the allocator opens: the shards are sized from the geometry
/// and the free lists are rebuilt from the scan, so appending blocks is only free
/// between one boot and the next.
pub fn grow_if_needed(path: &Path, cfg: &crate::config::Config) -> io::Result<()> {
    let mut g = read_geometry(path)?;
    let want = wanted(cfg);
    let have: [u64; 2] = CLASSES.map(|c| g.mblocks(c));
    let add: [u64; 2] = std::array::from_fn(|i| want[i].saturating_sub(have[i]));

    let f = open_direct(path, true)?.meter(Arc::new(Limiter::new(
        cfg.node.device_max_iops,
        cfg.node.device_max_bytes_per_sec,
    )));
    if add != [0, 0] {
        // Place both runs before writing anything, so the size the device has to reach
        // is known up front and a device that cannot reach it costs no IO.
        for (i, class) in CLASSES.into_iter().enumerate() {
            if add[i] > 0 {
                g.append(class, add[i])?;
            }
        }

        let bytes = device_bytes(&f)?;
        if g.total > bytes {
            extend(&f, g.total, bytes)?;
        }
        g.check(device_bytes(&f)?)?;

        let mut buf = Aligned::new(HUGE_PAGE as usize);
        for (i, class) in CLASSES.into_iter().enumerate() {
            write_empty(&f, &g, class, have[i], add[i], &mut buf)?;
        }
        // The blocks are on the device before a superblock names them; a run that got
        // this far and then lost power is simply not part of the device, and the next
        // boot places the same extent at the same offsets and writes it again.
        sync(&f)?;
    }
    save_geometry(&f, &g)
}

/// Write `g` through every superblock copy that does not already carry it, preserving
/// each copy's consensus record.
///
/// Copy 0 first, because a reader takes the first copy that decodes: a run cut short
/// leaves the newer geometry in front of the older ones, and calling this on every boot
/// is what finishes the job.
fn save_geometry(f: &Dev, g: &Geometry) -> io::Result<()> {
    let mut sb = Aligned::new(MBLOCK);
    let mut wrote = false;
    for i in 0..SB_COPIES {
        read_at(f, sb.as_mut(), sb_off(i))?;
        if Geometry::decode(sb.as_ref()).as_ref() == Some(g) {
            continue;
        }
        // A copy too damaged to patch is rebuilt whole. It loses that copy's consensus
        // record, which was unreadable anyway and which the next save restores.
        if !sb_valid(sb.as_ref()) {
            sb.as_mut().fill(0);
        }
        g.patch(sb.as_mut());
        write_at(f, sb.as_ref(), sb_off(i))?;
        wrote = true;
    }
    if wrote { sync(f) } else { Ok(()) }
}

/// How far short of `cfg` this geometry is, in pages, for the metric that says a restart
/// would grow the device. Zero when the config fits.
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
        "no valid superblock; device is not formatted",
    ))
}

// -------------------------------------------------------------------------- IO helpers

/// A page-aligned heap buffer, which O_DIRECT requires.
pub(crate) struct Aligned {
    ptr: *mut u8,
    len: usize,
}

impl Aligned {
    pub(crate) fn new(len: usize) -> Aligned {
        let layout = std::alloc::Layout::from_size_align(len, 4096).unwrap();
        // Safety: `len` is a nonzero multiple of the alignment at every call site.
        let ptr = unsafe { std::alloc::alloc(layout) };
        assert!(!ptr.is_null());
        Aligned { ptr, len }
    }

    pub(crate) fn as_ref(&self) -> &[u8] {
        unsafe { std::slice::from_raw_parts(self.ptr, self.len) }
    }

    pub(crate) fn as_mut(&mut self) -> &mut [u8] {
        unsafe { std::slice::from_raw_parts_mut(self.ptr, self.len) }
    }
}

impl Drop for Aligned {
    fn drop(&mut self) {
        let layout = std::alloc::Layout::from_size_align(self.len, 4096).unwrap();
        unsafe { std::alloc::dealloc(self.ptr, layout) };
    }
}

/// An open device, used only by control-plane paths: format, the superblock reads, and
/// the startup scan. The hot path goes through the runtime's `Disk` instead. Under
/// simulation this names an entry in the simulator's device table, so those paths run
/// unchanged against an in-memory image. The limiter, when present, is shared: the scan
/// drives one device from several threads and they meter against one budget between them.
#[cfg(not(feature = "sim"))]
pub(crate) struct Dev(std::fs::File, Option<Arc<Limiter>>);

#[cfg(feature = "sim")]
pub(crate) struct Dev(u32, Option<Arc<Limiter>>);

impl Dev {
    /// Pace this handle's transfers against a shared budget.
    pub(crate) fn meter(self, limit: Arc<Limiter>) -> Dev {
        Dev(self.0, Some(limit))
    }

    /// Wait out whatever the budget owes before a transfer of `len` bytes. Blocking,
    /// which is what these callers want: they have a whole thread to themselves. Under
    /// simulation it is a no-op: the clock there only moves when the event loop runs,
    /// and these paths run before it does.
    fn pace(&self, len: usize) {
        #[cfg(feature = "sim")]
        let _ = (&self.1, len);
        #[cfg(not(feature = "sim"))]
        if let Some(d) = self.1.as_ref().and_then(|l| l.admit(len as u32)) {
            std::thread::sleep(d);
        }
    }
}

#[cfg(not(feature = "sim"))]
pub(crate) fn open_direct(path: &Path, write: bool) -> io::Result<Dev> {
    use std::os::unix::fs::OpenOptionsExt;
    Ok(Dev(
        std::fs::OpenOptions::new()
            .read(true)
            .write(write)
            .custom_flags(libc::O_DIRECT)
            .open(path)?,
        None,
    ))
}

#[cfg(feature = "sim")]
pub(crate) fn open_direct(path: &Path, _write: bool) -> io::Result<Dev> {
    Ok(Dev(crate::sim::device(path)?, None))
}

/// Capacity of a block device, or the length of a regular file.
#[cfg(not(feature = "sim"))]
fn device_bytes(d: &Dev) -> io::Result<u64> {
    let fd = d.0.as_raw_fd();
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    if unsafe { libc::fstat(fd, &mut st) } < 0 {
        return Err(io::Error::last_os_error());
    }
    if st.st_mode & libc::S_IFMT == libc::S_IFBLK {
        const BLKGETSIZE64: libc::c_ulong = 0x8008_1272;
        let mut n: u64 = 0;
        if unsafe { libc::ioctl(fd, BLKGETSIZE64, &mut n) } < 0 {
            return Err(io::Error::last_os_error());
        }
        return Ok(n);
    }
    Ok(st.st_size as u64)
}

#[cfg(feature = "sim")]
fn device_bytes(d: &Dev) -> io::Result<u64> {
    crate::sim::device_bytes(d.0)
}

/// Grow the backing store to `len`. A regular file is extended in place; a block device
/// is whatever the operator gave us, so the shortfall is reported instead.
#[cfg(not(feature = "sim"))]
fn extend(d: &Dev, len: u64, have: u64) -> io::Result<()> {
    let mut st: libc::stat = unsafe { std::mem::zeroed() };
    if unsafe { libc::fstat(d.0.as_raw_fd(), &mut st) } < 0 {
        return Err(io::Error::last_os_error());
    }
    if st.st_mode & libc::S_IFMT == libc::S_IFBLK {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!(
                "config needs {len} B, device has {have} B; grow the device by {} B first",
                len - have
            ),
        ));
    }
    d.0.set_len(len)
}

#[cfg(feature = "sim")]
fn extend(_d: &Dev, len: u64, have: u64) -> io::Result<()> {
    Err(io::Error::new(
        io::ErrorKind::InvalidInput,
        format!("config needs {len} B, device has {have} B"),
    ))
}

/// Put everything written so far on stable storage. Both `format` and `grow` depend on
/// the order: a superblock naming blocks the device has not got yet is a device that
/// cannot be opened.
#[cfg(not(feature = "sim"))]
fn sync(d: &Dev) -> io::Result<()> {
    if unsafe { libc::fdatasync(d.0.as_raw_fd()) } < 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

/// The simulator's device table is itself the stable copy; a crash there drops nothing
/// that a real `fdatasync` would have saved.
#[cfg(feature = "sim")]
fn sync(_d: &Dev) -> io::Result<()> {
    Ok(())
}

pub(crate) fn write_at(d: &Dev, b: &[u8], off: u64) -> io::Result<()> {
    d.pace(b.len());
    #[cfg(feature = "sim")]
    return crate::sim::raw_write(d.0, off, b);
    #[cfg(not(feature = "sim"))]
    {
        let n = unsafe { libc::pwrite(d.0.as_raw_fd(), b.as_ptr() as *const _, b.len(), off as i64) };
        if n < 0 {
            return Err(io::Error::last_os_error());
        }
        if n as usize != b.len() {
            return Err(io::Error::new(io::ErrorKind::WriteZero, "short write"));
        }
        Ok(())
    }
}

pub(crate) fn read_at(d: &Dev, b: &mut [u8], off: u64) -> io::Result<()> {
    d.pace(b.len());
    #[cfg(feature = "sim")]
    return crate::sim::raw_read(d.0, off, b);
    #[cfg(not(feature = "sim"))]
    {
        let n = unsafe { libc::pread(d.0.as_raw_fd(), b.as_mut_ptr() as *mut _, b.len(), off as i64) };
        if n < 0 {
            return Err(io::Error::last_os_error());
        }
        if n as usize != b.len() {
            return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "short read"));
        }
        Ok(())
    }
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
        assert_eq!(MB_HDR + K_SMALL as usize * ENTRY_SMALL, MBLOCK);
        assert_eq!(MB_HDR + K_HUGE as usize * ENTRY_HUGE, MBLOCK);
    }

    #[test]
    fn crc32c_matches_known_vectors() {
        assert_eq!(crc32c(b""), 0);
        assert_eq!(crc32c(b"123456789"), 0xe306_9283);
        assert_eq!(crc32c(b"The quick brown fox jumps over the lazy dog"), 0x22620404);
        // The software and hardware paths must agree. The hardware path has two
        // interleaved block sizes, a u64 loop and a byte tail, so sweep lengths across
        // all of them.
        let data: Vec<u8> = (0..9000u32).map(|i| (i * 7) as u8).collect();
        for n in (0..data.len()).step_by(7) {
            assert_eq!(crc32c_sw(0, &data[..n]), crc32c(&data[..n]), "len {n}");
        }
        // Seeding composes.
        assert_eq!(crc32c_with(crc32c(&data[..37]), &data[37..]), crc32c(&data));
    }

    #[test]
    fn mblock_round_trips_and_detects_damage() {
        for class in [Class::Small, Class::Huge] {
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
            let h = Header { mblock_id: 11, generation: 5, class, live: 1 };
            put_mblock(&mut b, h, &e);

            let got = get_header(&b).unwrap();
            assert_eq!(got.mblock_id, 11);
            assert_eq!(got.generation, 5);
            assert_eq!(got.class, class);
            assert_eq!(got.live, 1);
            let mut want = e[3];
            if class == Class::Huge {
                want.data_crc = 0; // huge entries have no field to hold it
            }
            assert_eq!(get_entry(&b, class, 3), want);
            assert_eq!(get_entry(&b, class, 4), Entry::default());

            // A single flipped bit anywhere in the block is caught.
            b[MBLOCK - 1] ^= 1;
            assert!(get_header(&b).is_none());
            b[MBLOCK - 1] ^= 1;
            b[MB_HDR] ^= 0x80;
            assert!(get_header(&b).is_none());
        }
    }

    fn test_config() -> crate::config::Config {
        crate::config::Config::parse(
            "node id=1 zone=1 device=/dev/x cache_4k=4194304 cache_4m=8388608
             group 1 2 3
             volume 1 slot=0
               extent pages=5000 kind=lww zone=1
             volume 2 slot=1
               extent pages=3 kind=immutable_4m zone=1",
        )
        .unwrap()
    }

    #[test]
    fn geometry_round_trips() {
        let cfg = test_config();
        let g = Geometry::plan(64 << 30, &cfg).unwrap();
        assert!(g.slots(Class::Small) >= 5000 && g.slots(Class::Huge) >= 3);
        assert_eq!(g.slots(Class::Small), g.mblocks(Class::Small) * K_SMALL as u64);
        assert_eq!(g.extents(Class::Huge)[0].data % HUGE_PAGE, 0);

        let mut b = vec![0u8; MBLOCK];
        g.encode(&mut b);
        assert_eq!(u16(&b[4..]), FMT_VER);
        assert_eq!(Geometry::decode(&b).unwrap(), g);
        b[100] ^= 1;
        assert!(Geometry::decode(&b).is_none());

        // Too small a device is refused rather than laid out short.
        assert!(Geometry::plan(1 << 20, &cfg).is_err());
    }

    /// Every byte range a layout hands out, so growth can be checked for overlap without
    /// enumerating millions of slots.
    fn ranges(g: &Geometry) -> Vec<(u64, u64)> {
        let mut v = vec![
            (0, SB_REGION),
            (g.zero_base, g.zero_base + ZERO_BYTES),
            (g.cache_small, g.cache_small + g.cache_small_bytes),
            (g.cache_huge, g.cache_huge + g.cache_huge_bytes),
        ];
        for class in CLASSES {
            let mut first = 0;
            for e in g.extents(class) {
                // Both copies, and the slots those blocks name.
                v.push((e.meta, e.meta + 2 * e.mblocks * MBLOCK as u64));
                let slots = e.mblocks * class.k() as u64;
                v.push((e.data, e.data + slots * class.bytes()));
                // The accessors agree with the extent's own bounds.
                if e.mblocks > 0 {
                    let last = (first + e.mblocks - 1) as u32;
                    assert_eq!(g.mblock_off(class, first as u32, 0), e.meta);
                    assert_eq!(g.mblock_off(class, last, 1), v[v.len() - 2].1 - MBLOCK as u64);
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
        g.append(Class::Small, 40).unwrap();
        g.append(Class::Huge, 2).unwrap();
        g.append(Class::Small, 7).unwrap();

        // Nothing that was already placed moved, so the pages on the device are still
        // where their entries say they are.
        assert_eq!(g.extents(Class::Small)[0], g0.extents(Class::Small)[0]);
        assert_eq!(g.extents(Class::Huge)[0], g0.extents(Class::Huge)[0]);
        assert_eq!((g.zero_base, g.cache_small, g.cache_huge), (g0.zero_base, g0.cache_small, g0.cache_huge));
        assert!(g.total > g0.total);

        // Ids simply continue, and every range is disjoint and on the device.
        assert_eq!(g.mblocks(Class::Small), g0.mblocks(Class::Small) + 47);
        assert_eq!(g.mblocks(Class::Huge), g0.mblocks(Class::Huge) + 2);
        let v = ranges(&g);
        for (i, a) in v.iter().enumerate() {
            assert!(a.1 <= g.total, "{a:?} runs past the end");
            for b in &v[i + 1..] {
                assert!(a.1 <= b.0 || b.1 <= a.0, "{a:?} overlaps {b:?}");
            }
        }

        // A grown layout is written at its own version, so a build without this code
        // refuses the device instead of misreading it.
        let mut b = vec![0u8; MBLOCK];
        g.encode(&mut b);
        assert_eq!(u16(&b[4..]), FMT_VER_EXT);
        assert_eq!(Geometry::decode(&b).unwrap(), g);

        // Patching a live superblock leaves the consensus record alone.
        let c = Consensus { terms: vec![(7, 9)], ..Consensus::default() };
        c.patch(&mut b);
        g.append(Class::Huge, 1).unwrap();
        g.patch(&mut b);
        assert_eq!(Geometry::decode(&b).unwrap(), g);
        assert_eq!(Consensus::decode(&b).terms, vec![(7, 9)]);
    }

    #[test]
    fn growth_is_bounded() {
        let mut g = Geometry::plan(1 << 40, &test_config()).unwrap();
        for _ in 1..MAX_EXT {
            g.append(Class::Huge, 1).unwrap();
        }
        assert!(g.append(Class::Huge, 1).is_err());

        // A class whose slots would outgrow a 32-bit id is refused, not truncated.
        let mut g = Geometry::plan(1 << 40, &test_config()).unwrap();
        g.append(Class::Small, u32::MAX as u64 / K_SMALL as u64 + 1).unwrap();
        assert!(g.check(u64::MAX).is_err());
    }
}
