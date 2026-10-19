//! Extent volume: slot allocator and copy-on-write B+tree index.
//!
//! See `designs/racer-extent-volume.md`.

mod format;

pub use format::{
    CLASSES, KEY_BYTES, Key, MAX_SLOTS, META_FIRST_NODE_UNIT, META_SLOT, MIN_SLOTS, NodeAddr,
    RETAIN_FOREVER, SLOT_BYTES, SLOT_UNITS, SUPERBLOCK_A_UNIT, SUPERBLOCK_B_UNIT, Tier, UNIT_BYTES,
    VALUE_BYTES, Value, WHOLE_SLOT_CLASS, checksum, class_for, class_records, class_units,
    unit_offset,
};
