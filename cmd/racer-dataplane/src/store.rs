//! Worker-local encrypted slab storage. No HTTP, plaintext, or origin credentials.
pub mod checkpoint;
pub mod checkpoint_format;
pub mod direct;
pub mod eviction;
pub mod format;
pub mod index;
pub mod reader;
pub mod recovery;
pub mod segment;
pub mod slab;
pub mod writer;

use std::rc::Rc;
pub struct Store {
    pub reader: Rc<reader::StoreReader>,
    pub writer: Rc<writer::StoreWriter>,
    pub checkpoint: checkpoint::Checkpointer,
    pub recovery: recovery::Recovery,
    pub eviction: Rc<eviction::SegmentClock>,
}
