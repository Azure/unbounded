//! Newest-valid checkpoint selection with empty-cache fallback and no payload scan.
//!
//! Seal recovered open segments and discard uncheckpointed entries. Validate actual
//! direct-I/O geometry. A checksum-valid index can still reference stale payloads;
//! read validation and AEAD turn those into misses. Crash loss is unbounded.
use super::{
    checkpoint_format::{CheckpointImage, ShardImage},
    direct::DirectAlignment,
    index::Index,
    segment::Segments,
};
use crate::error::{Operation, deferred};
use std::{path::PathBuf, rc::Rc};
pub struct Recovery {
    directory: PathBuf,
    index: Rc<Index>,
    segments: Rc<Segments>,
}
impl Recovery {
    pub fn new(directory: PathBuf, index: Rc<Index>, segments: Rc<Segments>) -> Self {
        Self {
            directory,
            index,
            segments,
        }
    }
    pub fn load(&self, _alignment: DirectAlignment) -> Operation<'_, Option<CheckpointImage>> {
        deferred("recovery.load")
    }
    /// Before serving, validate shard ownership, geometry, descriptor agreement,
    /// catalog bounds, and segment generations together. Install the same cut into
    /// this worker's live index/segments; seal open segments, restore no freshness.
    /// None initializes an empty shard. Never fall back to scanning payloads.
    pub fn install_shard(&self, _image: Option<ShardImage>) -> Operation<'_, ()> {
        deferred("recovery.install_shard")
    }
}
#[cfg(test)]
mod tests { /* Both corrupt, newest torn, stale geometry, no slab-sized work/zeroing. */
}
