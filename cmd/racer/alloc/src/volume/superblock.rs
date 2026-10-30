//! Superblock codec. See `designs/racer-extent-volume.md`.
//!
//! The meta slot holds two superblocks, A in unit 0 and B in unit 1. A commit
//! writes the one that does not hold the newest durable superblock. A
//! superblock publishes one root set: the roots of the index, record map, and
//! slot table trees, the retired log of the commit that wrote it, and the
//! generation and incarnation high-water marks.

use super::btree::TreeRoot;
use super::format::{NodeAddr, SLOT_UNITS, UNIT_BYTES, checksum};

/// Superblock magic.
pub const SUPERBLOCK_MAGIC: [u8; 8] = *b"RCRVOLSB";

/// Superblock format version.
pub const SUPERBLOCK_VERSION: u16 = 2;

const VERSION_AT: usize = 8;
const VOLUME_ID_AT: usize = 16;
const SEQ_AT: usize = 32;
const UNIT_BYTES_AT: usize = 40;
const SLOT_UNITS_AT: usize = 44;
const SLOT_COUNT_AT: usize = 48;
const NEXT_GENERATION_AT: usize = 56;
const NEXT_INCARNATION_AT: usize = 64;
const TREES_AT: usize = 72;
const TREE_BYTES: usize = 32;
const LOG_HEAD_AT: usize = TREES_AT + 3 * TREE_BYTES;
const LOG_ENTRIES_AT: usize = LOG_HEAD_AT + 8;
const CHECKSUM_AT: usize = LOG_ENTRIES_AT + 8;
const END: usize = CHECKSUM_AT + 8;

/// Decoded superblock fields.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Superblock {
    /// Volume id, random at format.
    pub volume_id: [u8; 16],

    /// Commit seq. Newer superblocks have larger seqs.
    pub seq: u64,

    /// Unit size the volume was formatted with.
    pub unit_bytes: u32,

    /// Units per slot the volume was formatted with.
    pub slot_units: u32,

    /// Slots in the volume when the superblock was written.
    pub slot_count: u64,

    /// Generation high-water mark: no committed entry has a generation at or
    /// above it.
    pub next_generation: u64,

    /// Next slot incarnation to hand out.
    pub next_incarnation: u32,

    /// Roots of the index, record map, and slot table, in that order.
    pub trees: [TreeRoot; 3],

    /// First page of the retired log: space this commit stopped
    /// referencing, reusable once a later commit is durable. `NONE` when
    /// empty.
    pub log_head: NodeAddr,

    /// Entries in the retired log.
    pub log_entries: u64,
}

impl Superblock {
    /// Superblock of an empty volume with this build's geometry.
    pub fn empty(volume_id: [u8; 16], seq: u64, slot_count: u64) -> Self {
        Self {
            volume_id,
            seq,
            unit_bytes: UNIT_BYTES as u32,
            slot_units: SLOT_UNITS,
            slot_count,
            next_generation: 1,
            next_incarnation: 1,
            trees: [TreeRoot::EMPTY; 3],
            log_head: NodeAddr::NONE,
            log_entries: 0,
        }
    }

    /// Whether the unit size and slot size match this build.
    pub fn has_current_geometry(&self) -> bool {
        self.unit_bytes == UNIT_BYTES as u32 && self.slot_units == SLOT_UNITS
    }

    /// Encode into `buf` (one unit).
    pub fn encode_into(&self, buf: &mut [u8]) {
        buf.fill(0);
        buf[..VERSION_AT].copy_from_slice(&SUPERBLOCK_MAGIC);
        buf[VERSION_AT..VERSION_AT + 2].copy_from_slice(&SUPERBLOCK_VERSION.to_le_bytes());
        buf[VOLUME_ID_AT..SEQ_AT].copy_from_slice(&self.volume_id);
        buf[SEQ_AT..UNIT_BYTES_AT].copy_from_slice(&self.seq.to_le_bytes());
        buf[UNIT_BYTES_AT..SLOT_UNITS_AT].copy_from_slice(&self.unit_bytes.to_le_bytes());
        buf[SLOT_UNITS_AT..SLOT_COUNT_AT].copy_from_slice(&self.slot_units.to_le_bytes());
        buf[SLOT_COUNT_AT..NEXT_GENERATION_AT].copy_from_slice(&self.slot_count.to_le_bytes());
        buf[NEXT_GENERATION_AT..NEXT_INCARNATION_AT]
            .copy_from_slice(&self.next_generation.to_le_bytes());
        buf[NEXT_INCARNATION_AT..NEXT_INCARNATION_AT + 4]
            .copy_from_slice(&self.next_incarnation.to_le_bytes());
        for (i, tree) in self.trees.iter().enumerate() {
            let at = TREES_AT + i * TREE_BYTES;
            buf[at..at + 8].copy_from_slice(&tree.root[0].0.to_le_bytes());
            buf[at + 8..at + 16].copy_from_slice(&tree.root[1].0.to_le_bytes());
            buf[at + 16] = tree.level;
            buf[at + 24..at + 32].copy_from_slice(&tree.count.to_le_bytes());
        }
        buf[LOG_HEAD_AT..LOG_ENTRIES_AT].copy_from_slice(&self.log_head.0.to_le_bytes());
        buf[LOG_ENTRIES_AT..CHECKSUM_AT].copy_from_slice(&self.log_entries.to_le_bytes());
        let sum = checksum(&buf[..UNIT_BYTES]);
        buf[CHECKSUM_AT..END].copy_from_slice(&sum.to_le_bytes());
    }

    /// Encode into one unit.
    pub fn encode(&self) -> Box<[u8; UNIT_BYTES]> {
        let mut buf = Box::new([0u8; UNIT_BYTES]);
        self.encode_into(&mut buf[..]);
        buf
    }

    /// Decode one unit. Checks the magic, version, checksum, that reserved
    /// bytes are zero, and that empty trees are consistent. Does not check
    /// the geometry or the slot count.
    pub fn decode(bytes: &[u8; UNIT_BYTES]) -> Option<Self> {
        if bytes[..VERSION_AT] != SUPERBLOCK_MAGIC
            || u16::from_le_bytes([bytes[VERSION_AT], bytes[VERSION_AT + 1]]) != SUPERBLOCK_VERSION
        {
            return None;
        }
        let mut reserved = vec![
            VERSION_AT + 2..VOLUME_ID_AT,
            NEXT_INCARNATION_AT + 4..TREES_AT,
            END..UNIT_BYTES,
        ];
        for i in 0..3 {
            let at = TREES_AT + i * TREE_BYTES;
            reserved.push(at + 17..at + 24);
        }
        if reserved
            .into_iter()
            .any(|range| bytes[range].iter().any(|&b| b != 0))
        {
            return None;
        }
        let u64_at = |at: usize| u64::from_le_bytes(bytes[at..at + 8].try_into().expect("8 bytes"));
        let u32_at = |at: usize| u32::from_le_bytes(bytes[at..at + 4].try_into().expect("4 bytes"));
        let mut copy = *bytes;
        copy[CHECKSUM_AT..END].fill(0);
        if checksum(&copy) != u64_at(CHECKSUM_AT) {
            return None;
        }
        let mut trees = [TreeRoot::EMPTY; 3];
        for (i, tree) in trees.iter_mut().enumerate() {
            let at = TREES_AT + i * TREE_BYTES;
            *tree = TreeRoot {
                root: [NodeAddr(u64_at(at)), NodeAddr(u64_at(at + 8))],
                level: bytes[at + 16],
                count: u64_at(at + 24),
            };
            if tree.root[0].is_none()
                && (!tree.root[1].is_none() || tree.level != 0 || tree.count != 0)
            {
                return None;
            }
        }
        let superblock = Self {
            volume_id: bytes[VOLUME_ID_AT..SEQ_AT].try_into().expect("16 bytes"),
            seq: u64_at(SEQ_AT),
            unit_bytes: u32_at(UNIT_BYTES_AT),
            slot_units: u32_at(SLOT_UNITS_AT),
            slot_count: u64_at(SLOT_COUNT_AT),
            next_generation: u64_at(NEXT_GENERATION_AT),
            next_incarnation: u32_at(NEXT_INCARNATION_AT),
            trees,
            log_head: NodeAddr(u64_at(LOG_HEAD_AT)),
            log_entries: u64_at(LOG_ENTRIES_AT),
        };
        if superblock.log_head.is_none() != (superblock.log_entries == 0) {
            return None;
        }
        Some(superblock)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample() -> Superblock {
        let mut sb = Superblock::empty([7; 16], 42, 64);
        sb.next_generation = 1 << 41;
        sb.next_incarnation = 77;
        sb.trees[0] = TreeRoot {
            root: [NodeAddr::new(0, 4), NodeAddr::new(3, 9)],
            level: 2,
            count: 1234,
        };
        sb.trees[2] = TreeRoot {
            root: [NodeAddr::new(0, 5), NodeAddr::NONE],
            level: 0,
            count: 3,
        };
        sb.log_head = NodeAddr::new(0, 6);
        sb.log_entries = 9;
        sb
    }

    #[test]
    fn round_trip_and_layout() {
        let sb = sample();
        let bytes = sb.encode();
        assert_eq!(Superblock::decode(&bytes), Some(sb));
        assert_eq!(&bytes[..8], b"RCRVOLSB");
        assert_eq!(u16::from_le_bytes([bytes[8], bytes[9]]), 2);
        assert_eq!(bytes[16..32], [7; 16]);
        assert_eq!(u64::from_le_bytes(bytes[32..40].try_into().unwrap()), 42);
        assert_eq!(u32::from_le_bytes(bytes[40..44].try_into().unwrap()), 4096);
        assert_eq!(u32::from_le_bytes(bytes[44..48].try_into().unwrap()), 4104);
        assert_eq!(u64::from_le_bytes(bytes[48..56].try_into().unwrap()), 64);
        assert_eq!(
            u64::from_le_bytes(bytes[56..64].try_into().unwrap()),
            1 << 41
        );
        assert_eq!(u32::from_le_bytes(bytes[64..68].try_into().unwrap()), 77);
        assert_eq!(bytes[72 + 16], 2);
        assert!(bytes[END..].iter().all(|&b| b == 0));
        let empty = Superblock::empty([1; 16], 1, 4);
        assert_eq!(Superblock::decode(&empty.encode()), Some(empty));
        assert!(empty.has_current_geometry());
    }

    #[test]
    fn every_single_bit_flip_is_rejected() {
        let bytes = sample().encode();
        for bit in 0..UNIT_BYTES * 8 {
            let mut copy = *bytes;
            copy[bit / 8] ^= 1 << (bit % 8);
            assert_eq!(Superblock::decode(&copy), None, "bit {bit}");
        }
    }

    #[test]
    fn rejects_inconsistent_fields() {
        let mut sb = Superblock::empty([1; 16], 1, 4);
        sb.trees[1].root[1] = NodeAddr::new(1, 1);
        assert_eq!(Superblock::decode(&sb.encode()), None);
        let mut sb = Superblock::empty([1; 16], 1, 4);
        sb.log_entries = 3;
        assert_eq!(Superblock::decode(&sb.encode()), None);
        assert_eq!(Superblock::decode(&[0; UNIT_BYTES]), None);
    }
}
