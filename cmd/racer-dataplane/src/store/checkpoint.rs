//! Consistent logical cuts and alternating two-generation checkpoint publication.
//!
//! Freeze or lease index/allocation/generation state together. Exclude dirty writes.
//! Prevent segment reuse from invalidating a snapshot under construction. Specify
//! write/rename ordering before implementation; no fsync durability is promised.
use super::{checkpoint_format::ShardImage, index::Index, segment::Segments};
use crate::error::{Operation, deferred};
use std::{path::PathBuf, rc::Rc};
pub struct Checkpointer {
    directory: PathBuf,
    index: Rc<Index>,
    segments: Rc<Segments>,
}
impl Checkpointer {
    pub fn new(directory: PathBuf, index: Rc<Index>, segments: Rc<Segments>) -> Self {
        Self {
            directory,
            index,
            segments,
        }
    }
    pub fn snapshot_shard(&self) -> Operation<'_, ShardImage> {
        deferred("checkpoint.snapshot_shard")
    }
    /// Called once by the application coordinator after every shard is frozen.
    pub fn publish(&self, _shards: Vec<ShardImage>) -> Operation<'_, ()> {
        deferred("checkpoint.publish")
    }
}
#[cfg(test)]
mod tests { /* Mixed-generation prevention, alternating publication, torn writes. */
}
