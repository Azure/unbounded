//! Extent volume: slot allocator and copy-on-write B+tree index.
//!
//! See `designs/racer-extent-volume.md`.

mod format;
mod node;
mod slots;
mod store;
mod superblock;
mod tree;

pub use format::{
    CLASSES, KEY_BYTES, Key, MAX_SLOTS, META_FIRST_NODE_UNIT, META_SLOT, MIN_SLOTS, NodeAddr,
    RETAIN_FOREVER, SLOT_BYTES, SLOT_UNITS, SUPERBLOCK_A_UNIT, SUPERBLOCK_B_UNIT, Tier, UNIT_BYTES,
    VALUE_BYTES, Value, WHOLE_SLOT_CLASS, checksum, class_for, class_records, class_units,
    unit_offset,
};
pub use node::{
    DecodedNode, Expect, INNER_CAPACITY, LEAF_CAPACITY, NODE_HEADER_BYTES, NODE_MAGIC,
    NODE_VERSION, NodeBody, decode as decode_node, encode_inner, encode_leaf,
};
pub use slots::{Allocation, Eviction, Limits, SlotKind, SlotLease, SlotState, Slots, Usage};
pub use store::{
    ChargeFn, CommitStats, GENERATION_SKIP, IO_CONCURRENCY, MountReport, Reservation, Volume,
    VolumeOptions,
};
pub use superblock::{SUPERBLOCK_MAGIC, SUPERBLOCK_VERSION, Superblock};
pub use tree::{CommitPlan, LoadReport, Loader, Range, Tree};
