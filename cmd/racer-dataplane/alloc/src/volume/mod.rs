//! Extent volume: slot allocator and disk-resident copy-on-write B+trees
//! with a bounded page cache.
//!
//! See `designs/racer-extent-volume.md`.

mod btree;
mod commit;
mod evict;
mod format;
mod mount;
mod page;
mod pager;
mod store;
mod superblock;
mod table;

pub use btree::TreeRoot;
pub use format::{
    CLASSES, KEY_BYTES, Key, MAX_SLOTS, META_FIRST_NODE_UNIT, META_SLOT, MIN_SLOTS,
    NODE_FIRST_UNIT, NodeAddr, RETAIN_FOREVER, SLOT_BYTES, SLOT_UNITS, SUPERBLOCK_A_UNIT,
    SUPERBLOCK_B_UNIT, Tier, UNIT_BYTES, VALUE_BYTES, Value, WHOLE_SLOT_CLASS, bitmap_unit,
    checksum, class_for, class_records, class_units, first_page_unit, unit_offset,
};
pub use page::{
    BITMAP_MAGIC, HEADER_BYTES, INNER_CAPACITY, LOG_CAPACITY, LOG_MAGIC, PAGE_MAGIC, PAGE_VERSION,
    RecordDesc, Retired, SlotSummary, TreeKind,
};
pub use pager::{FRAME_BYTES, PagerStats};
pub use store::{
    ChargeFn, CommitStats, DEFAULT_METADATA_CACHE_BYTES, EVICT_BATCH, GENERATION_SKIP,
    IO_CONCURRENCY, MountReport, Reservation, ScrubReport, Volume, VolumeOptions,
};
pub use superblock::{SUPERBLOCK_MAGIC, SUPERBLOCK_VERSION, Superblock};
pub use table::{Limits, RUN_PAGES, SlotKind, SlotLease, SlotState, Usage};
