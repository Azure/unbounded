//! Superblock codec. See `designs/racer-extent-volume.md`.
//!
//! The meta slot holds two superblocks, A in unit 0 and B in unit 1. A commit
//! writes the one that does not hold the newest durable superblock.

use super::format::{NodeAddr, SLOT_UNITS, UNIT_BYTES, checksum};

/// Superblock magic.
pub const SUPERBLOCK_MAGIC: [u8; 8] = *b"RCRVOLSB";

/// Superblock format version.
pub const SUPERBLOCK_VERSION: u16 = 1;

const VERSION_AT: usize = 8;
const VOLUME_ID_AT: usize = 16;
const SEQ_AT: usize = 32;
const UNIT_BYTES_AT: usize = 40;
const SLOT_UNITS_AT: usize = 44;
const SLOT_COUNT_AT: usize = 48;
const ROOT0_AT: usize = 56;
const ROOT1_AT: usize = 64;
const ROOT_LEVEL_AT: usize = 72;
const ENTRIES_AT: usize = 80;
const NEXT_GENERATION_AT: usize = 88;
const CHECKSUM_AT: usize = 96;
const END: usize = 104;

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

    /// Root addresses. `NONE` twice for an empty tree; the second is `NONE`
    /// for a leaf root.
    pub root: [NodeAddr; 2],

    /// Level of the root node.
    pub root_level: u8,

    /// Entries in the tree.
    pub entries: u64,

    /// Generation high-water mark: no committed entry has a generation at or
    /// above it.
    pub next_generation: u64,
}

impl Superblock {
    /// Superblock of an empty tree with this volume's geometry.
    pub fn empty(volume_id: [u8; 16], seq: u64, slot_count: u64, next_generation: u64) -> Self {
        Self {
            volume_id,
            seq,
            unit_bytes: UNIT_BYTES as u32,
            slot_units: SLOT_UNITS,
            slot_count,
            root: [NodeAddr::NONE; 2],
            root_level: 0,
            entries: 0,
            next_generation,
        }
    }

    /// Whether the unit size and slot size match this build.
    pub fn has_current_geometry(&self) -> bool {
        self.unit_bytes == UNIT_BYTES as u32 && self.slot_units == SLOT_UNITS
    }

    /// Encode into one unit.
    pub fn encode(&self) -> Box<[u8; UNIT_BYTES]> {
        let mut buf = Box::new([0u8; UNIT_BYTES]);
        buf[..VERSION_AT].copy_from_slice(&SUPERBLOCK_MAGIC);
        buf[VERSION_AT..VERSION_AT + 2].copy_from_slice(&SUPERBLOCK_VERSION.to_le_bytes());
        buf[VOLUME_ID_AT..SEQ_AT].copy_from_slice(&self.volume_id);
        buf[SEQ_AT..UNIT_BYTES_AT].copy_from_slice(&self.seq.to_le_bytes());
        buf[UNIT_BYTES_AT..SLOT_UNITS_AT].copy_from_slice(&self.unit_bytes.to_le_bytes());
        buf[SLOT_UNITS_AT..SLOT_COUNT_AT].copy_from_slice(&self.slot_units.to_le_bytes());
        buf[SLOT_COUNT_AT..ROOT0_AT].copy_from_slice(&self.slot_count.to_le_bytes());
        buf[ROOT0_AT..ROOT1_AT].copy_from_slice(&self.root[0].0.to_le_bytes());
        buf[ROOT1_AT..ROOT_LEVEL_AT].copy_from_slice(&self.root[1].0.to_le_bytes());
        buf[ROOT_LEVEL_AT] = self.root_level;
        buf[ENTRIES_AT..NEXT_GENERATION_AT].copy_from_slice(&self.entries.to_le_bytes());
        buf[NEXT_GENERATION_AT..CHECKSUM_AT].copy_from_slice(&self.next_generation.to_le_bytes());
        let sum = checksum(&buf[..]);
        buf[CHECKSUM_AT..END].copy_from_slice(&sum.to_le_bytes());
        buf
    }

    /// Decode one unit. Checks the magic, version, checksum, that reserved
    /// bytes are zero, and that an empty root has no second address. Does not
    /// check the geometry or the slot count.
    pub fn decode(bytes: &[u8; UNIT_BYTES]) -> Option<Self> {
        if bytes[..VERSION_AT] != SUPERBLOCK_MAGIC
            || u16::from_le_bytes([bytes[VERSION_AT], bytes[VERSION_AT + 1]]) != SUPERBLOCK_VERSION
        {
            return None;
        }
        let reserved = [
            VERSION_AT + 2..VOLUME_ID_AT,
            ROOT_LEVEL_AT + 1..ENTRIES_AT,
            END..UNIT_BYTES,
        ];
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
        let superblock = Self {
            volume_id: bytes[VOLUME_ID_AT..SEQ_AT].try_into().expect("16 bytes"),
            seq: u64_at(SEQ_AT),
            unit_bytes: u32_at(UNIT_BYTES_AT),
            slot_units: u32_at(SLOT_UNITS_AT),
            slot_count: u64_at(SLOT_COUNT_AT),
            root: [NodeAddr(u64_at(ROOT0_AT)), NodeAddr(u64_at(ROOT1_AT))],
            root_level: bytes[ROOT_LEVEL_AT],
            entries: u64_at(ENTRIES_AT),
            next_generation: u64_at(NEXT_GENERATION_AT),
        };
        if superblock.root[0].is_none()
            && (!superblock.root[1].is_none()
                || superblock.root_level != 0
                || superblock.entries != 0)
        {
            return None;
        }
        Some(superblock)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample() -> Superblock {
        Superblock {
            volume_id: [7; 16],
            seq: 42,
            unit_bytes: UNIT_BYTES as u32,
            slot_units: SLOT_UNITS,
            slot_count: 64,
            root: [NodeAddr::new(0, 2), NodeAddr::new(3, 9)],
            root_level: 2,
            entries: 1234,
            next_generation: 1 << 41,
        }
    }

    #[test]
    fn round_trip_and_layout() {
        let sb = sample();
        let bytes = sb.encode();
        assert_eq!(Superblock::decode(&bytes), Some(sb));
        assert_eq!(&bytes[..8], b"RCRVOLSB");
        assert_eq!(u16::from_le_bytes([bytes[8], bytes[9]]), 1);
        assert_eq!(bytes[16..32], [7; 16]);
        assert_eq!(u64::from_le_bytes(bytes[32..40].try_into().unwrap()), 42);
        assert_eq!(u32::from_le_bytes(bytes[40..44].try_into().unwrap()), 4096);
        assert_eq!(u32::from_le_bytes(bytes[44..48].try_into().unwrap()), 4104);
        assert_eq!(u64::from_le_bytes(bytes[48..56].try_into().unwrap()), 64);
        assert_eq!(
            u64::from_le_bytes(bytes[56..64].try_into().unwrap()),
            NodeAddr::new(0, 2).0
        );
        assert_eq!(
            u64::from_le_bytes(bytes[64..72].try_into().unwrap()),
            NodeAddr::new(3, 9).0
        );
        assert_eq!(bytes[72], 2);
        assert_eq!(u64::from_le_bytes(bytes[80..88].try_into().unwrap()), 1234);
        assert_eq!(
            u64::from_le_bytes(bytes[88..96].try_into().unwrap()),
            1 << 41
        );
        let mut zeroed = *bytes;
        zeroed[96..104].fill(0);
        assert_eq!(
            u64::from_le_bytes(bytes[96..104].try_into().unwrap()),
            checksum(&zeroed)
        );
        assert!(bytes[104..].iter().all(|&b| b == 0));
        let empty = Superblock::empty([1; 16], 1, 4, 1);
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
    fn rejects_nonzero_reserved_and_bad_empty_root() {
        for at in [10, 15, 73, 79, 104, 4095] {
            let mut bytes = *sample().encode();
            bytes[at] = 1;
            bytes[96..104].fill(0);
            let sum = checksum(&bytes);
            bytes[96..104].copy_from_slice(&sum.to_le_bytes());
            assert_eq!(Superblock::decode(&bytes), None, "offset {at}");
        }
        let mut sb = Superblock::empty([1; 16], 1, 4, 1);
        sb.root[1] = NodeAddr::new(1, 1);
        assert_eq!(Superblock::decode(&sb.encode()), None);
        assert_eq!(Superblock::decode(&[0; UNIT_BYTES]), None);
    }
}
