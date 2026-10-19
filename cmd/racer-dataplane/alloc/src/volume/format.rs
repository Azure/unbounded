//! Shared on-disk geometry, keys, and values for the extent volume.
//!
//! See `designs/racer-extent-volume.md` for the format.

use std::fmt;

/// Bytes in one unit. Every on-disk object starts on a unit boundary.
pub const UNIT_BYTES: usize = 4096;

/// Units in one slot.
pub const SLOT_UNITS: u32 = 4104;

/// Bytes in one slot: 16 MiB plus 32 KiB for a record header and tag.
pub const SLOT_BYTES: u64 = SLOT_UNITS as u64 * UNIT_BYTES as u64;

/// Fewest slots a volume may have.
pub const MIN_SLOTS: u64 = 4;

/// Most slots a volume may have (about 1 PiB).
pub const MAX_SLOTS: u64 = 1 << 26;

/// Slot that holds the superblocks and the first tree nodes.
pub const META_SLOT: u32 = 0;

/// Unit of superblock A in the meta slot.
pub const SUPERBLOCK_A_UNIT: u16 = 0;

/// Unit of superblock B in the meta slot.
pub const SUPERBLOCK_B_UNIT: u16 = 1;

/// First unit in the meta slot that may hold a tree node.
pub const META_FIRST_NODE_UNIT: u16 = 2;

/// Size classes in units. The last class is a whole slot.
pub const CLASSES: [u32; 23] = [
    1, 2, 3, 4, 6, 8, 12, 16, 24, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1536, 2048,
    SLOT_UNITS,
];

/// Index of the whole-slot class.
pub const WHOLE_SLOT_CLASS: u8 = (CLASSES.len() - 1) as u8;

/// Bytes in a key.
pub const KEY_BYTES: usize = 24;

/// Bytes in an encoded value.
pub const VALUE_BYTES: usize = 32;

/// `retain_until` value that never expires.
pub const RETAIN_FOREVER: u64 = u64::MAX;

/// Smallest class index whose records hold `length` bytes.
pub fn class_for(length: u64) -> Option<u8> {
    if length == 0 {
        return None;
    }
    let units = length.div_ceil(UNIT_BYTES as u64);
    CLASSES
        .iter()
        .position(|&class| u64::from(class) >= units)
        .map(|index| index as u8)
}

/// Units used by one record of a class.
pub fn class_units(class: u8) -> Option<u32> {
    CLASSES.get(usize::from(class)).copied()
}

/// Records that fit in one slot of a class.
pub fn class_records(class: u8) -> Option<u32> {
    class_units(class).map(|units| SLOT_UNITS / units)
}

/// Byte offset of a unit in the slab address space.
pub fn unit_offset(slot: u32, unit: u16) -> u64 {
    u64::from(slot) * SLOT_BYTES + u64::from(unit) * UNIT_BYTES as u64
}

/// CRC-64 used for nodes and superblocks.
pub fn checksum(bytes: &[u8]) -> u64 {
    let mut digest = crc64fast::Digest::new();
    digest.write(bytes);
    digest.sum64()
}

/// Address of one tree node: `slot << 16 | unit`.
#[derive(Clone, Copy, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct NodeAddr(pub u64);

impl NodeAddr {
    /// No address.
    pub const NONE: Self = Self(u64::MAX);

    /// Address of a unit in a slot.
    pub fn new(slot: u32, unit: u16) -> Self {
        Self(u64::from(slot) << 16 | u64::from(unit))
    }

    /// Whether this is `NONE`.
    pub fn is_none(self) -> bool {
        self == Self::NONE
    }

    /// Slot part of the address.
    pub fn slot(self) -> u32 {
        (self.0 >> 16) as u32
    }

    /// Unit part of the address.
    pub fn unit(self) -> u16 {
        self.0 as u16
    }

    /// Byte offset in the slab address space.
    pub fn offset(self) -> u64 {
        unit_offset(self.slot(), self.unit())
    }

    /// Whether the address can exist in a volume with `slot_count` slots.
    pub fn valid_for(self, slot_count: u64) -> bool {
        !self.is_none()
            && u64::from(self.slot()) < slot_count
            && u32::from(self.unit()) < SLOT_UNITS
            && !(self.slot() == META_SLOT && self.unit() < META_FIRST_NODE_UNIT)
    }
}

impl fmt::Debug for NodeAddr {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        if self.is_none() {
            f.write_str("NodeAddr(NONE)")
        } else {
            write!(f, "NodeAddr({}:{})", self.slot(), self.unit())
        }
    }
}

/// Index key: cache hash (8), version hash (12), big-endian page number (4).
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct Key(pub [u8; KEY_BYTES]);

impl Key {
    /// Smallest key.
    pub const MIN: Self = Self([0; KEY_BYTES]);

    /// Largest key.
    pub const MAX: Self = Self([0xff; KEY_BYTES]);

    /// Build a key from its parts.
    pub fn new(cache: [u8; 8], version: [u8; 12], page: u32) -> Self {
        let mut bytes = [0; KEY_BYTES];
        bytes[..8].copy_from_slice(&cache);
        bytes[8..20].copy_from_slice(&version);
        bytes[20..].copy_from_slice(&page.to_be_bytes());
        Self(bytes)
    }

    /// Cache hash part.
    pub fn cache(&self) -> [u8; 8] {
        self.0[..8].try_into().expect("fixed key layout")
    }

    /// Version hash part.
    pub fn version(&self) -> [u8; 12] {
        self.0[8..20].try_into().expect("fixed key layout")
    }

    /// Page number part.
    pub fn page(&self) -> u32 {
        u32::from_be_bytes(self.0[20..].try_into().expect("fixed key layout"))
    }
}

/// Durability tier of a record.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub enum Tier {
    /// Best effort. May be evicted at any time.
    Cache = 0,

    /// Durable. Kept until deleted or `retain_until` passes.
    Storage = 1,
}

impl Tier {
    /// Decode a stored tier byte.
    pub fn from_byte(byte: u8) -> Option<Self> {
        match byte {
            0 => Some(Self::Cache),
            1 => Some(Self::Storage),
            _ => None,
        }
    }
}

/// Index value: where a record is and how long it is kept.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Value {
    /// Data slot.
    pub slot: u32,

    /// First unit of the record in the slot.
    pub unit: u16,

    /// Size class index.
    pub class: u8,

    /// Tier.
    pub tier: Tier,

    /// Record bytes written.
    pub length: u32,

    /// Reserved, 0.
    pub flags: u32,

    /// Write id, also stored in the record header.
    pub generation: u64,

    /// Unix seconds until which a storage record is protected.
    pub retain_until: u64,
}

impl Value {
    /// Encode as 32 little-endian bytes.
    pub fn encode(&self) -> [u8; VALUE_BYTES] {
        let mut bytes = [0; VALUE_BYTES];
        bytes[0..4].copy_from_slice(&self.slot.to_le_bytes());
        bytes[4..6].copy_from_slice(&self.unit.to_le_bytes());
        bytes[6] = self.class;
        bytes[7] = self.tier as u8;
        bytes[8..12].copy_from_slice(&self.length.to_le_bytes());
        bytes[12..16].copy_from_slice(&self.flags.to_le_bytes());
        bytes[16..24].copy_from_slice(&self.generation.to_le_bytes());
        bytes[24..32].copy_from_slice(&self.retain_until.to_le_bytes());
        bytes
    }

    /// Decode 32 little-endian bytes. Checks the tier, class, and that the
    /// record fits in its class and slot. Does not check the slot count.
    pub fn decode(bytes: &[u8; VALUE_BYTES]) -> Option<Self> {
        let value = Self {
            slot: u32::from_le_bytes(bytes[0..4].try_into().ok()?),
            unit: u16::from_le_bytes(bytes[4..6].try_into().ok()?),
            class: bytes[6],
            tier: Tier::from_byte(bytes[7])?,
            length: u32::from_le_bytes(bytes[8..12].try_into().ok()?),
            flags: u32::from_le_bytes(bytes[12..16].try_into().ok()?),
            generation: u64::from_le_bytes(bytes[16..24].try_into().ok()?),
            retain_until: u64::from_le_bytes(bytes[24..32].try_into().ok()?),
        };
        value.is_well_formed().then_some(value)
    }

    /// Whether the fields are consistent: known class, nonzero length that fits
    /// the class, unit aligned to the class, record inside the slot, nonzero
    /// generation, and not in the meta slot.
    pub fn is_well_formed(&self) -> bool {
        let Some(units) = class_units(self.class) else {
            return false;
        };
        self.length != 0
            && u64::from(self.length) <= u64::from(units) * UNIT_BYTES as u64
            && u32::from(self.unit) % units == 0
            && u32::from(self.unit) + units <= SLOT_UNITS
            && self.generation != 0
            && self.slot != META_SLOT
    }

    /// Byte offset of the record in the slab address space.
    pub fn offset(&self) -> u64 {
        unit_offset(self.slot, self.unit)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn value() -> Value {
        Value {
            slot: 7,
            unit: 12,
            class: 6,
            tier: Tier::Storage,
            length: 40_000,
            flags: 0,
            generation: 99,
            retain_until: RETAIN_FOREVER,
        }
    }

    #[test]
    fn classes_are_sorted_and_whole_slot_last() {
        assert!(CLASSES.windows(2).all(|pair| pair[0] < pair[1]));
        assert_eq!(class_units(WHOLE_SLOT_CLASS), Some(SLOT_UNITS));
        assert_eq!(class_records(WHOLE_SLOT_CLASS), Some(1));
        assert_eq!(class_records(0), Some(SLOT_UNITS));
    }

    #[test]
    fn class_for_picks_smallest_fit() {
        assert_eq!(class_for(0), None);
        assert_eq!(class_for(1), Some(0));
        assert_eq!(class_for(4096), Some(0));
        assert_eq!(class_for(4097), Some(1));
        assert_eq!(class_for(5 * 4096), Some(4));
        assert_eq!(class_for(SLOT_BYTES), Some(WHOLE_SLOT_CLASS));
        assert_eq!(class_for(SLOT_BYTES + 1), None);
        let largest_record = 16 * 1024 * 1024 + 16 * 1024 + 16;
        assert_eq!(class_for(largest_record), Some(WHOLE_SLOT_CLASS));
    }

    #[test]
    fn value_round_trips() {
        let value = value();
        assert_eq!(Value::decode(&value.encode()), Some(value));
    }

    #[test]
    fn value_rejects_bad_fields() {
        let cases: [fn(&mut Value); 7] = [
            |v| v.class = 23,
            |v| v.length = 0,
            |v| v.length = 48 * 4096 + 1,
            |v| v.unit = 13,
            |v| {
                v.class = 7;
                v.unit = 4096;
            },
            |v| v.generation = 0,
            |v| v.slot = META_SLOT,
        ];
        for mutate in cases {
            let mut bad = value();
            mutate(&mut bad);
            assert_eq!(Value::decode(&bad.encode()), None, "{bad:?}");
        }
        let mut bytes = value().encode();
        bytes[7] = 2;
        assert_eq!(Value::decode(&bytes), None);
    }

    #[test]
    fn node_addr_parts_and_validity() {
        let addr = NodeAddr::new(3, 17);
        assert_eq!((addr.slot(), addr.unit()), (3, 17));
        assert_eq!(addr.offset(), 3 * SLOT_BYTES + 17 * 4096);
        assert!(addr.valid_for(4));
        assert!(!addr.valid_for(3));
        assert!(!NodeAddr::NONE.valid_for(u64::MAX));
        assert!(!NodeAddr::new(META_SLOT, SUPERBLOCK_B_UNIT).valid_for(4));
        assert!(NodeAddr::new(META_SLOT, META_FIRST_NODE_UNIT).valid_for(4));
        assert!(!NodeAddr::new(1, 4104).valid_for(4));
    }

    #[test]
    fn key_parts_and_order() {
        let key = Key::new([1; 8], [2; 12], 0x0102_0304);
        assert_eq!(key.cache(), [1; 8]);
        assert_eq!(key.version(), [2; 12]);
        assert_eq!(key.page(), 0x0102_0304);
        assert!(Key::new([1; 8], [2; 12], 1) < Key::new([1; 8], [2; 12], 256));
        assert!(Key::MIN < key && key < Key::MAX);
    }
}
