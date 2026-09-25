//! Newest-valid checkpoint selection with empty-cache fallback and no payload scan.
//!
//! Seal recovered open segments and discard uncheckpointed entries. Validate actual
//! direct-I/O geometry. A checksum-valid index can still reference stale payloads;
//! read validation and AEAD turn those into misses. Crash loss is unbounded.
use super::{checkpoint_format::CheckpointImage, direct::DirectAlignment};
use crate::error::{Operation, deferred};
use std::path::PathBuf;
pub struct Recovery {
    directory: PathBuf,
}
impl Recovery {
    pub fn new(directory: PathBuf) -> Self {
        Self { directory }
    }
    pub fn load(&self, _alignment: DirectAlignment) -> Operation<'_, Option<CheckpointImage>> {
        deferred("recovery.load")
    }
}
#[cfg(test)]
mod tests { /* Both corrupt, newest torn, stale geometry, no slab-sized work/zeroing. */
}
