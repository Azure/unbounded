//! Bounded versioned checkpoint encoding, SHA-256 over the complete logical image.
use super::{index::IndexSnapshot, segment::SegmentSnapshot};
use crate::{
    error::{Result, pending},
    model::identity::WorkerId,
};
pub struct ShardImage {
    pub worker: WorkerId,
    pub index: IndexSnapshot,
    pub segments: Vec<SegmentSnapshot>,
}
pub struct CheckpointImage {
    pub version: u32,
    pub sequence: u64,
    pub shards: Vec<ShardImage>,
}
pub struct CheckpointCodec;
impl CheckpointCodec {
    pub fn encode(&self, _image: &CheckpointImage) -> Result<Vec<u8>> {
        pending("checkpoint.encode")
    }
    pub fn decode(&self, _bytes: &[u8]) -> Result<CheckpointImage> {
        pending("checkpoint.decode")
    }
}
#[cfg(test)]
mod tests { /* Checksums, bounds, unknown versions, offsets, metadata-only objects. */
}
