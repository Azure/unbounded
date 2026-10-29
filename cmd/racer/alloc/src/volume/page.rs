//! Metadata page codecs: tree pages, node-slot bitmaps, and retired logs.
//!
//! See the "Pages" section of `designs/racer-extent-volume.md`. Every page is
//! one 4096 byte unit with a 64 byte header that names its kind, the volume,
//! the commit that wrote it, and its own address, sealed by a CRC-64. A page is
//! checked as a whole before it is cached, so corruption stays inside one page.

use super::format::{
    KEY_BYTES, Key, NodeAddr, SLOT_UNITS, UNIT_BYTES, VALUE_BYTES, Value, checksum,
};

/// Magic of tree pages.
pub const PAGE_MAGIC: [u8; 8] = *b"RCRVPAGE";

/// Magic of node-slot bitmap pages.
pub const BITMAP_MAGIC: [u8; 8] = *b"RCRVBMAP";

/// Magic of retired-log pages.
pub const LOG_MAGIC: [u8; 8] = *b"RCRVRLOG";

/// Version of every metadata page format.
pub const PAGE_VERSION: u16 = 2;

/// Bytes in a page header.
pub const HEADER_BYTES: usize = 64;

/// Bytes in the largest tree value.
pub const RAW_BYTES: usize = 40;

/// Raw tree value. Only the first [`TreeKind::value_bytes`] bytes are used.
pub type Raw = [u8; RAW_BYTES];

/// Children per inner page.
pub const INNER_CAPACITY: usize = (UNIT_BYTES - HEADER_BYTES) / (KEY_BYTES + 16);

/// Entries per retired-log page.
pub const LOG_CAPACITY: usize = (UNIT_BYTES - HEADER_BYTES) / LOG_ENTRY_BYTES;

/// Words in a node-slot bitmap.
pub const BITMAP_WORDS: usize = (SLOT_UNITS as usize).div_ceil(64);

const LOG_ENTRY_BYTES: usize = 16;
const CHECKSUM_AT: usize = 48;
const KIND_AT: usize = 10;
const LEVEL_AT: usize = 11;
const COUNT_AT: usize = 12;

/// Which tree a page belongs to. Each kind has its own key and value rules.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
#[repr(u8)]
pub enum TreeKind {
    /// Index: [`Key`] to [`Value`].
    Index = 1,

    /// Record map: (slot, unit) to the record's key, generation, and slot
    /// incarnation. See [`RecordDesc`].
    Records = 2,

    /// Slot table: slot to [`SlotSummary`].
    Slots = 3,
}

impl TreeKind {
    /// Every kind, in superblock order.
    pub const ALL: [Self; 3] = [Self::Index, Self::Records, Self::Slots];

    fn from_byte(byte: u8) -> Option<Self> {
        match byte {
            1 => Some(Self::Index),
            2 => Some(Self::Records),
            3 => Some(Self::Slots),
            _ => None,
        }
    }

    /// Position in the superblock's root table.
    pub fn index(self) -> usize {
        self as usize - 1
    }

    /// Bytes of one value.
    pub fn value_bytes(self) -> usize {
        match self {
            Self::Index | Self::Slots => VALUE_BYTES,
            Self::Records => RAW_BYTES,
        }
    }

    /// Entries per leaf.
    pub fn leaf_capacity(self) -> usize {
        (UNIT_BYTES - HEADER_BYTES) / (KEY_BYTES + self.value_bytes())
    }

    /// Whether a key may appear in a tree of this kind.
    pub fn key_ok(self, key: &Key) -> bool {
        match self {
            Self::Index => true,
            Self::Records => {
                key.0[6..].iter().all(|&b| b == 0)
                    && u32::from(u16::from_be_bytes([key.0[4], key.0[5]])) < SLOT_UNITS
            }
            Self::Slots => key.0[4..].iter().all(|&b| b == 0),
        }
    }

    /// Whether a value may appear in a tree of this kind.
    pub fn value_ok(self, value: &[u8]) -> bool {
        match self {
            Self::Index => Value::decode(value.try_into().expect("value bytes")).is_some(),
            Self::Records => RecordDesc::decode(value).is_some(),
            Self::Slots => SlotSummary::decode(value).is_some(),
        }
    }
}

/// Record map key of a record or unit.
pub fn record_key(slot: u32, unit: u16) -> Key {
    let mut key = [0; KEY_BYTES];
    key[..4].copy_from_slice(&slot.to_be_bytes());
    key[4..6].copy_from_slice(&unit.to_be_bytes());
    Key(key)
}

/// Slot and unit of a record map key.
pub fn record_key_parts(key: &Key) -> (u32, u16) {
    (
        u32::from_be_bytes(key.0[..4].try_into().expect("4 bytes")),
        u16::from_be_bytes([key.0[4], key.0[5]]),
    )
}

/// Slot table key of a slot.
pub fn slot_key(slot: u32) -> Key {
    let mut key = [0; KEY_BYTES];
    key[..4].copy_from_slice(&slot.to_be_bytes());
    Key(key)
}

/// Slot of a slot table key.
pub fn slot_key_slot(key: &Key) -> u32 {
    u32::from_be_bytes(key.0[..4].try_into().expect("4 bytes"))
}

/// Record map value: which index entry owns a record.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RecordDesc {
    /// Index key of the record.
    pub key: Key,

    /// Generation of the record.
    pub generation: u64,

    /// Incarnation of the slot when the record was published.
    pub incarnation: u32,
}

impl RecordDesc {
    /// Encode as 40 bytes.
    pub fn encode(&self) -> Raw {
        let mut raw = [0; RAW_BYTES];
        raw[..KEY_BYTES].copy_from_slice(&self.key.0);
        raw[24..32].copy_from_slice(&self.generation.to_le_bytes());
        raw[32..36].copy_from_slice(&self.incarnation.to_le_bytes());
        raw
    }

    /// Decode 40 bytes. Rejects a zero generation or incarnation and nonzero
    /// padding.
    pub fn decode(raw: &[u8]) -> Option<Self> {
        let desc = Self {
            key: Key(raw[..KEY_BYTES].try_into().ok()?),
            generation: u64::from_le_bytes(raw[24..32].try_into().ok()?),
            incarnation: u32::from_le_bytes(raw[32..36].try_into().ok()?),
        };
        (desc.generation != 0 && desc.incarnation != 0 && raw[36..40] == [0; 4]).then_some(desc)
    }
}

/// Persisted kind of a slot.
pub const SLOT_NODE: u8 = 1;

/// Persisted kind of a data slot.
pub const SLOT_DATA: u8 = 2;

/// Persisted kind of a quarantined slot: never allocated or evicted.
pub const SLOT_QUARANTINED: u8 = 3;

/// Slot table value. Free slots have no entry.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct SlotSummary {
    /// `SLOT_NODE`, `SLOT_DATA`, or `SLOT_QUARANTINED`.
    pub kind: u8,

    /// Tier byte of a data slot.
    pub tier: u8,

    /// Class of a data slot.
    pub class: u8,

    /// Incarnation, unique per initialization of any slot.
    pub incarnation: u32,

    /// Data slot: records with a record map entry of this incarnation.
    pub used: u32,

    /// Data slot: upper bound of the records' `retain_until`.
    pub retain_until: u64,

    /// Node slot: which bitmap copy (0 or 1) is current.
    pub copy: u8,

    /// Node slot: seq of the commit that wrote the current bitmap copy, 0
    /// when none was written.
    pub bitmap_seq: u64,
}

impl SlotSummary {
    /// Encode as 32 bytes.
    pub fn encode(&self) -> Raw {
        let mut raw = [0; RAW_BYTES];
        raw[0] = self.kind;
        raw[1] = self.tier;
        raw[2] = self.class;
        raw[3] = self.copy;
        raw[4..8].copy_from_slice(&self.incarnation.to_le_bytes());
        raw[8..12].copy_from_slice(&self.used.to_le_bytes());
        raw[16..24].copy_from_slice(&self.retain_until.to_le_bytes());
        raw[24..32].copy_from_slice(&self.bitmap_seq.to_le_bytes());
        raw
    }

    /// Decode 32 bytes and check the fields are consistent.
    pub fn decode(raw: &[u8]) -> Option<Self> {
        let summary = Self {
            kind: raw[0],
            tier: raw[1],
            class: raw[2],
            incarnation: u32::from_le_bytes(raw[4..8].try_into().ok()?),
            used: u32::from_le_bytes(raw[8..12].try_into().ok()?),
            retain_until: u64::from_le_bytes(raw[16..24].try_into().ok()?),
            copy: raw[3],
            bitmap_seq: u64::from_le_bytes(raw[24..32].try_into().ok()?),
        };
        if raw[12..16] != [0; 4] || summary.incarnation == 0 {
            return None;
        }
        let empty = summary.tier == 0
            && summary.class == 0
            && summary.used == 0
            && summary.retain_until == 0;
        let ok = match summary.kind {
            SLOT_NODE => empty && summary.copy < 2,
            SLOT_QUARANTINED => empty && summary.copy == 0 && summary.bitmap_seq == 0,
            SLOT_DATA => {
                summary.tier < 2
                    && super::format::class_records(summary.class)
                        .is_some_and(|records| summary.used <= records)
                    && summary.copy == 0
                    && summary.bitmap_seq == 0
            }
            _ => false,
        };
        ok.then_some(summary)
    }
}

fn u16_at(bytes: &[u8], at: usize) -> u16 {
    u16::from_le_bytes([bytes[at], bytes[at + 1]])
}

fn u32_at(bytes: &[u8], at: usize) -> u32 {
    u32::from_le_bytes(bytes[at..at + 4].try_into().expect("4 bytes"))
}

fn u64_at(bytes: &[u8], at: usize) -> u64 {
    u64::from_le_bytes(bytes[at..at + 8].try_into().expect("8 bytes"))
}

#[allow(clippy::too_many_arguments)]
fn header(
    buf: &mut [u8],
    magic: &[u8; 8],
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
    kind: u8,
    level: u8,
    count: usize,
) {
    buf.fill(0);
    buf[0..8].copy_from_slice(magic);
    buf[8..10].copy_from_slice(&PAGE_VERSION.to_le_bytes());
    buf[KIND_AT] = kind;
    buf[LEVEL_AT] = level;
    buf[COUNT_AT..COUNT_AT + 2].copy_from_slice(&(count as u16).to_le_bytes());
    buf[16..32].copy_from_slice(volume_id);
    buf[32..40].copy_from_slice(&seq.to_le_bytes());
    buf[40..48].copy_from_slice(&addr.0.to_le_bytes());
}

fn seal(buf: &mut [u8]) {
    let sum = checksum(buf);
    buf[CHECKSUM_AT..CHECKSUM_AT + 8].copy_from_slice(&sum.to_le_bytes());
}

/// Whether the common header fields and checksum are right.
fn sealed(bytes: &[u8], magic: &[u8; 8], volume_id: &[u8; 16], addr: NodeAddr) -> bool {
    if bytes.len() != UNIT_BYTES
        || bytes[0..8] != *magic
        || u16_at(bytes, 8) != PAGE_VERSION
        || bytes[16..32] != *volume_id
        || u64_at(bytes, 40) != addr.0
    {
        return false;
    }
    let mut digest = crc64fast::Digest::new();
    digest.write(&bytes[..CHECKSUM_AT]);
    digest.write(&[0; 8]);
    digest.write(&bytes[CHECKSUM_AT + 8..]);
    digest.sum64() == u64_at(bytes, CHECKSUM_AT)
}

/// Encode a leaf of `kind` into `buf` (one unit). Panics if `entries` is empty
/// or above the kind's leaf capacity.
pub fn encode_leaf<'a>(
    buf: &mut [u8],
    kind: TreeKind,
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
    entries: impl ExactSizeIterator<Item = (&'a Key, &'a Raw)>,
) {
    let count = entries.len();
    assert!(count > 0 && count <= kind.leaf_capacity());
    header(buf, &PAGE_MAGIC, volume_id, seq, addr, kind as u8, 0, count);
    let width = KEY_BYTES + kind.value_bytes();
    for (i, (key, value)) in entries.enumerate() {
        let at = HEADER_BYTES + i * width;
        buf[at..at + KEY_BYTES].copy_from_slice(&key.0);
        buf[at + KEY_BYTES..at + width].copy_from_slice(&value[..kind.value_bytes()]);
    }
    seal(buf);
}

/// Encode an inner page into `buf`. Panics if `entries` is empty, above
/// [`INNER_CAPACITY`], or `level` is 0.
pub fn encode_inner(
    buf: &mut [u8],
    kind: TreeKind,
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
    level: u8,
    entries: &[(Key, [NodeAddr; 2])],
) {
    assert!(!entries.is_empty() && entries.len() <= INNER_CAPACITY && level > 0);
    header(
        buf,
        &PAGE_MAGIC,
        volume_id,
        seq,
        addr,
        kind as u8,
        level,
        entries.len(),
    );
    for (i, (key, child)) in entries.iter().enumerate() {
        let at = HEADER_BYTES + i * (KEY_BYTES + 16);
        buf[at..at + KEY_BYTES].copy_from_slice(&key.0);
        buf[at + KEY_BYTES..at + KEY_BYTES + 8].copy_from_slice(&child[0].0.to_le_bytes());
        buf[at + KEY_BYTES + 8..at + KEY_BYTES + 16].copy_from_slice(&child[1].0.to_le_bytes());
    }
    seal(buf);
}

/// A validated tree page. Accessors read the raw bytes in place.
///
/// The bytes are inline, so a cached `Rc<TreePage>` reaches its header
/// without a second pointer chase.
pub struct TreePage {
    bytes: [u8; UNIT_BYTES],
}

impl std::fmt::Debug for TreePage {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TreePage")
            .field("kind", &self.kind())
            .field("level", &self.level())
            .field("count", &self.count())
            .field("seq", &self.seq())
            .finish()
    }
}

impl TreePage {
    /// Validate a tree page read from `addr`: magic, version, checksum,
    /// volume id, own address, kind, level, count, key order, keys and
    /// values allowed for the kind, child addresses, and zero padding. Does
    /// not check the seq, the level expected by the parent, or the key range;
    /// see [`TreePage::fits`].
    pub fn decode(
        bytes: Box<[u8; UNIT_BYTES]>,
        volume_id: &[u8; 16],
        addr: NodeAddr,
        slot_count: u64,
    ) -> Option<Self> {
        if !sealed(&bytes[..], &PAGE_MAGIC, volume_id, addr)
            || u16_at(&bytes[..], 14) != 0
            || u64_at(&bytes[..], 56) != 0
        {
            return None;
        }
        let kind = TreeKind::from_byte(bytes[KIND_AT])?;
        let level = bytes[LEVEL_AT];
        let count = usize::from(u16_at(&bytes[..], COUNT_AT));
        let (capacity, width) = if level == 0 {
            (kind.leaf_capacity(), KEY_BYTES + kind.value_bytes())
        } else {
            (INNER_CAPACITY, KEY_BYTES + 16)
        };
        if count == 0 || count > capacity {
            return None;
        }
        let end = HEADER_BYTES + count * width;
        if bytes[end..].iter().any(|&b| b != 0) {
            return None;
        }
        let page = Self { bytes: *bytes };
        let mut previous: Option<&[u8]> = None;
        for i in 0..count {
            let at = HEADER_BYTES + i * width;
            let key = &page.bytes[at..at + KEY_BYTES];
            if previous.is_some_and(|p| p >= key) {
                return None;
            }
            previous = Some(key);
            if !kind.key_ok(&page.key(i)) {
                return None;
            }
            if level == 0 {
                if !kind.value_ok(page.value(i)) {
                    return None;
                }
            } else {
                let [first, second] = page.child(i);
                if !first.valid_for(slot_count)
                    || !(second.is_none() || second.valid_for(slot_count))
                {
                    return None;
                }
            }
        }
        Some(page)
    }

    /// A page this process just encoded, without validation.
    pub fn trusted(bytes: Box<[u8; UNIT_BYTES]>) -> Self {
        Self { bytes: *bytes }
    }

    /// Raw bytes.
    #[cfg(test)]
    pub fn bytes(&self) -> &[u8; UNIT_BYTES] {
        &self.bytes
    }

    /// Tree kind.
    pub fn kind(&self) -> TreeKind {
        TreeKind::from_byte(self.bytes[KIND_AT]).expect("validated kind")
    }

    /// Level, 0 for leaves.
    pub fn level(&self) -> u8 {
        self.bytes[LEVEL_AT]
    }

    /// Entries.
    pub fn count(&self) -> usize {
        usize::from(u16_at(&self.bytes[..], COUNT_AT))
    }

    /// Commit that wrote the page.
    pub fn seq(&self) -> u64 {
        u64_at(&self.bytes[..], 32)
    }

    fn width(&self) -> usize {
        if self.level() == 0 {
            KEY_BYTES + self.kind().value_bytes()
        } else {
            KEY_BYTES + 16
        }
    }

    /// Key of entry `i`.
    pub fn key(&self, i: usize) -> Key {
        let at = HEADER_BYTES + i * self.width();
        Key(self.bytes[at..at + KEY_BYTES]
            .try_into()
            .expect("key bytes"))
    }

    /// Value bytes of leaf entry `i`.
    pub fn value(&self, i: usize) -> &[u8] {
        let at = HEADER_BYTES + i * self.width() + KEY_BYTES;
        &self.bytes[at..at + self.kind().value_bytes()]
    }

    /// Value of leaf entry `i` as a raw buffer.
    pub fn raw(&self, i: usize) -> Raw {
        let mut raw = [0; RAW_BYTES];
        let value = self.value(i);
        raw[..value.len()].copy_from_slice(value);
        raw
    }

    /// Child addresses of inner entry `i`.
    pub fn child(&self, i: usize) -> [NodeAddr; 2] {
        let at = HEADER_BYTES + i * self.width() + KEY_BYTES;
        [
            NodeAddr(u64_at(&self.bytes[..], at)),
            NodeAddr(u64_at(&self.bytes[..], at + 8)),
        ]
    }

    /// Position of `key` among the entries, as `binary_search` reports it.
    pub fn search(&self, key: &Key) -> Result<usize, usize> {
        let width = self.width();
        let bytes = &self.bytes[..];
        let (mut low, mut high) = (0, self.count());
        while low < high {
            let mid = (low + high) / 2;
            let at = HEADER_BYTES + mid * width;
            let probe: &[u8; KEY_BYTES] = bytes[at..at + KEY_BYTES].try_into().expect("key bytes");
            match super::format::compare_keys(probe, &key.0) {
                std::cmp::Ordering::Less => low = mid + 1,
                std::cmp::Ordering::Greater => high = mid,
                std::cmp::Ordering::Equal => return Ok(mid),
            }
        }
        Err(low)
    }

    /// Index of the child of an inner page that covers `key`.
    pub fn child_index(&self, key: &Key) -> usize {
        match self.search(key) {
            Ok(i) => i,
            Err(i) => i.saturating_sub(1),
        }
    }

    /// Whether the page fits where its parent expects it: kind, level, seq
    /// not above `max_seq`, and keys inside `[low, high)`.
    pub fn fits(
        &self,
        kind: TreeKind,
        level: u8,
        max_seq: u64,
        low: &Key,
        high: Option<&Key>,
    ) -> bool {
        self.kind() == kind
            && self.level() == level
            && self.seq() <= max_seq
            && self.key(0) >= *low
            && high.is_none_or(|high| self.key(self.count() - 1) < *high)
    }
}

/// Encode a node-slot bitmap into `buf`.
pub fn encode_bitmap(
    buf: &mut [u8],
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
    slot: u32,
    words: &[u64; BITMAP_WORDS],
) {
    header(buf, &BITMAP_MAGIC, volume_id, seq, addr, 0, 0, 0);
    buf[56..60].copy_from_slice(&slot.to_le_bytes());
    for (i, word) in words.iter().enumerate() {
        let at = HEADER_BYTES + i * 8;
        buf[at..at + 8].copy_from_slice(&word.to_le_bytes());
    }
    seal(buf);
}

/// Decode a node-slot bitmap for `slot` at `addr` written by a commit no
/// newer than `max_seq`. Returns the words and the seq that wrote them. Bits
/// past the slot's last unit must be clear.
pub fn decode_bitmap(
    bytes: &[u8],
    volume_id: &[u8; 16],
    max_seq: u64,
    addr: NodeAddr,
    slot: u32,
) -> Option<(Box<[u64; BITMAP_WORDS]>, u64)> {
    let seq = u64_at(bytes, 32);
    if !sealed(bytes, &BITMAP_MAGIC, volume_id, addr)
        || seq > max_seq
        || u32_at(bytes, 56) != slot
        || bytes[10..16].iter().any(|&b| b != 0)
        || bytes[60..64].iter().any(|&b| b != 0)
        || bytes[HEADER_BYTES + BITMAP_WORDS * 8..]
            .iter()
            .any(|&b| b != 0)
    {
        return None;
    }
    let mut words = Box::new([0u64; BITMAP_WORDS]);
    for (i, word) in words.iter_mut().enumerate() {
        *word = u64_at(bytes, HEADER_BYTES + i * 8);
    }
    let tail = SLOT_UNITS as usize % 64;
    if tail != 0 && words[BITMAP_WORDS - 1] >> tail != 0 {
        return None;
    }
    Some((words, seq))
}

/// One entry of a retired log: space a commit stopped referencing.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct Retired {
    /// Node unit or first unit of a record.
    pub addr: NodeAddr,

    /// For records, the slot incarnation that owned the record. 0 for
    /// metadata pages.
    pub incarnation: u32,

    /// Whether this is a storage record rather than a metadata page.
    pub record: bool,
}

/// Encode a retired-log page holding `entries`, chained to `next`.
pub fn encode_log(
    buf: &mut [u8],
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
    next: NodeAddr,
    entries: &[Retired],
) {
    assert!(entries.len() <= LOG_CAPACITY);
    header(buf, &LOG_MAGIC, volume_id, seq, addr, 0, 0, entries.len());
    buf[56..64].copy_from_slice(&next.0.to_le_bytes());
    for (i, entry) in entries.iter().enumerate() {
        let at = HEADER_BYTES + i * LOG_ENTRY_BYTES;
        buf[at..at + 8].copy_from_slice(&entry.addr.0.to_le_bytes());
        buf[at + 8..at + 12].copy_from_slice(&entry.incarnation.to_le_bytes());
        buf[at + 12] = u8::from(entry.record);
    }
    seal(buf);
}

/// Decode a retired-log page written by commit `seq`. Returns the entries
/// and the next page address.
pub fn decode_log(
    bytes: &[u8],
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
    slot_count: u64,
) -> Option<(Vec<Retired>, NodeAddr)> {
    if !sealed(bytes, &LOG_MAGIC, volume_id, addr) || u64_at(bytes, 32) != seq {
        return None;
    }
    let count = usize::from(u16_at(bytes, COUNT_AT));
    if count > LOG_CAPACITY
        || bytes[KIND_AT] != 0
        || bytes[LEVEL_AT] != 0
        || bytes[14..16].iter().any(|&b| b != 0)
        || bytes[HEADER_BYTES + count * LOG_ENTRY_BYTES..]
            .iter()
            .any(|&b| b != 0)
    {
        return None;
    }
    let next = NodeAddr(u64_at(bytes, 56));
    if !next.is_none() && !next.valid_for(slot_count) {
        return None;
    }
    let mut entries = Vec::with_capacity(count);
    for i in 0..count {
        let at = HEADER_BYTES + i * LOG_ENTRY_BYTES;
        let addr = NodeAddr(u64_at(bytes, at));
        let incarnation = u32_at(bytes, at + 8);
        let record = bytes[at + 12];
        if record > 1
            || bytes[at + 13..at + 16].iter().any(|&b| b != 0)
            || u64::from(addr.slot()) >= slot_count
            || u32::from(addr.unit()) >= SLOT_UNITS
            || (record == 1) != (incarnation != 0)
        {
            return None;
        }
        entries.push(Retired {
            addr,
            incarnation,
            record: record == 1,
        });
    }
    Some((entries, next))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::volume::format::{RETAIN_FOREVER, Tier};

    const VOLUME: [u8; 16] = [7; 16];

    fn key(n: u32) -> Key {
        Key::new([1; 8], [2; 12], n)
    }

    fn value(n: u64) -> Raw {
        let value = Value {
            slot: 3,
            unit: 0,
            class: 0,
            tier: Tier::Storage,
            length: 100,
            flags: 0,
            generation: n + 1,
            retain_until: RETAIN_FOREVER,
        };
        let mut raw = [0; RAW_BYTES];
        raw[..VALUE_BYTES].copy_from_slice(&value.encode());
        raw
    }

    fn leaf() -> (NodeAddr, Box<[u8; UNIT_BYTES]>) {
        let entries: Vec<(Key, Raw)> = (0..TreeKind::Index.leaf_capacity() as u32)
            .map(|i| (key(i * 3 + 1), value(u64::from(i))))
            .collect();
        let addr = NodeAddr::new(2, 9);
        let mut buf = Box::new([0u8; UNIT_BYTES]);
        encode_leaf(
            &mut buf[..],
            TreeKind::Index,
            &VOLUME,
            5,
            addr,
            entries.iter().map(|(k, v)| (k, v)),
        );
        (addr, buf)
    }

    fn inner() -> (NodeAddr, Box<[u8; UNIT_BYTES]>) {
        let entries: Vec<(Key, [NodeAddr; 2])> = (0..INNER_CAPACITY as u32)
            .map(|i| {
                let second = if i % 2 == 0 {
                    NodeAddr::NONE
                } else {
                    NodeAddr::new(4, 10 + i as u16)
                };
                (key(i * 5), [NodeAddr::new(1, 10 + i as u16), second])
            })
            .collect();
        let addr = NodeAddr::new(0, 9);
        let mut buf = Box::new([0u8; UNIT_BYTES]);
        encode_inner(&mut buf[..], TreeKind::Index, &VOLUME, 5, addr, 2, &entries);
        (addr, buf)
    }

    #[test]
    fn capacities() {
        assert_eq!(TreeKind::Index.leaf_capacity(), 72);
        assert_eq!(TreeKind::Records.leaf_capacity(), 63);
        assert_eq!(TreeKind::Slots.leaf_capacity(), 72);
        assert_eq!(INNER_CAPACITY, 100);
        assert_eq!(LOG_CAPACITY, 252);
    }

    #[test]
    fn tree_pages_round_trip_and_fit() {
        let (addr, bytes) = leaf();
        let page = TreePage::decode(bytes, &VOLUME, addr, 8).unwrap();
        assert_eq!(
            (page.kind(), page.level(), page.seq()),
            (TreeKind::Index, 0, 5)
        );
        assert_eq!(page.count(), 72);
        assert_eq!(page.key(1), key(4));
        assert_eq!(page.raw(1), value(1));
        assert_eq!(page.search(&key(4)), Ok(1));
        assert_eq!(page.search(&key(5)), Err(2));
        assert!(page.fits(TreeKind::Index, 0, 5, &key(1), Some(&key(1000))));
        assert!(!page.fits(TreeKind::Index, 0, 4, &key(1), None));
        assert!(!page.fits(TreeKind::Index, 0, 5, &key(2), None));
        assert!(!page.fits(TreeKind::Index, 0, 5, &key(0), Some(&key(214))));
        assert!(!page.fits(TreeKind::Records, 0, 5, &key(0), None));
        assert!(!page.fits(TreeKind::Index, 1, 5, &key(0), None));

        let (addr, bytes) = inner();
        let page = TreePage::decode(bytes, &VOLUME, addr, 8).unwrap();
        assert_eq!(page.level(), 2);
        assert_eq!(page.child(1), [NodeAddr::new(1, 11), NodeAddr::new(4, 11)]);
        assert_eq!(page.child_index(&key(7)), 1);
        assert_eq!(page.child_index(&Key::MIN), 0);
        // Child addresses must exist in the volume.
        let (addr, bytes) = inner();
        assert!(TreePage::decode(bytes, &VOLUME, addr, 4).is_none());
    }

    #[test]
    fn every_bit_flip_is_rejected() {
        for (addr, bytes) in [leaf(), inner()] {
            assert!(TreePage::decode(bytes.clone(), &VOLUME, addr, 8).is_some());
            let mut copy = bytes.clone();
            for bit in (0..UNIT_BYTES * 8).step_by(7) {
                copy[bit / 8] ^= 1 << (bit % 8);
                assert!(
                    TreePage::decode(copy.clone(), &VOLUME, addr, 8).is_none(),
                    "bit {bit}"
                );
                copy[bit / 8] ^= 1 << (bit % 8);
            }
        }
        let (addr, bytes) = leaf();
        assert!(TreePage::decode(bytes.clone(), &[8; 16], addr, 8).is_none());
        assert!(TreePage::decode(bytes, &VOLUME, NodeAddr::new(2, 10), 8).is_none());
    }

    #[test]
    fn descriptors_and_summaries_round_trip() {
        let desc = RecordDesc {
            key: key(9),
            generation: 77,
            incarnation: 3,
        };
        assert_eq!(RecordDesc::decode(&desc.encode()), Some(desc));
        let mut bad = desc.encode();
        bad[38] = 1;
        assert_eq!(RecordDesc::decode(&bad), None);
        assert_eq!(
            RecordDesc::decode(
                &RecordDesc {
                    generation: 0,
                    ..desc
                }
                .encode()
            ),
            None
        );
        let summary = SlotSummary {
            kind: SLOT_DATA,
            tier: 1,
            class: 4,
            incarnation: 5,
            used: 10,
            retain_until: 99,
            ..SlotSummary::default()
        };
        assert_eq!(SlotSummary::decode(&summary.encode()), Some(summary));
        let node = SlotSummary {
            kind: SLOT_NODE,
            incarnation: 2,
            copy: 1,
            bitmap_seq: 8,
            ..SlotSummary::default()
        };
        assert_eq!(SlotSummary::decode(&node.encode()), Some(node));
        for bad in [
            SlotSummary { kind: 9, ..summary },
            SlotSummary {
                incarnation: 0,
                ..summary
            },
            SlotSummary {
                used: 5000,
                ..summary
            },
            SlotSummary { used: 2, ..node },
            SlotSummary { copy: 2, ..node },
            SlotSummary {
                bitmap_seq: 3,
                ..summary
            },
        ] {
            assert_eq!(SlotSummary::decode(&bad.encode()), None, "{bad:?}");
        }
        assert_eq!(record_key_parts(&record_key(7, 300)), (7, 300));
        assert!(TreeKind::Records.key_ok(&record_key(7, 300)));
        assert!(!TreeKind::Records.key_ok(&record_key(7, 5000)));
        assert_eq!(slot_key_slot(&slot_key(12)), 12);
        assert!(record_key(1, 5) < record_key(2, 0));
    }

    #[test]
    fn bitmaps_and_logs_round_trip() {
        let mut words = [0u64; BITMAP_WORDS];
        words[0] = 0b1011;
        words[64] = 0xff;
        let addr = NodeAddr::new(3, 1);
        let mut buf = [0u8; UNIT_BYTES];
        encode_bitmap(&mut buf, &VOLUME, 9, addr, 3, &words);
        let (decoded, seq) = decode_bitmap(&buf, &VOLUME, 9, addr, 3).unwrap();
        assert_eq!((*decoded, seq), (words, 9));
        assert!(decode_bitmap(&buf, &VOLUME, 10, addr, 3).is_some());
        assert!(decode_bitmap(&buf, &VOLUME, 8, addr, 3).is_none());
        assert!(decode_bitmap(&buf, &VOLUME, 9, addr, 4).is_none());
        words[64] = 1 << 8;
        encode_bitmap(&mut buf, &VOLUME, 9, addr, 3, &words);
        assert!(decode_bitmap(&buf, &VOLUME, 9, addr, 3).is_none());

        let entries = vec![
            Retired {
                addr: NodeAddr::new(2, 5),
                incarnation: 0,
                record: false,
            },
            Retired {
                addr: NodeAddr::new(3, 12),
                incarnation: 4,
                record: true,
            },
        ];
        let addr = NodeAddr::new(2, 6);
        encode_log(&mut buf, &VOLUME, 4, addr, NodeAddr::new(2, 7), &entries);
        assert_eq!(
            decode_log(&buf, &VOLUME, 4, addr, 8),
            Some((entries.clone(), NodeAddr::new(2, 7)))
        );
        assert!(decode_log(&buf, &VOLUME, 5, addr, 8).is_none());
        buf[100] ^= 1;
        assert!(decode_log(&buf, &VOLUME, 4, addr, 8).is_none());
    }
}
