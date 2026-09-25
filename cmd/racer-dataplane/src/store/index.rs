//! Identity mappings and reverse segment membership, published after complete writes.
use super::{
    segment::{Generation, SegmentId},
    slab::SlabLocation,
};
use crate::{
    error::{Result, pending},
    model::{identity::PageId, metadata::ObjectMetadata},
};
#[derive(Clone, Debug)]
pub struct RecordLocation {
    pub segment: SegmentId,
    pub generation: Generation,
    pub location: SlabLocation,
}
pub struct Index;
pub struct IndexSnapshot {
    pub entries: Vec<(PageId, RecordLocation)>,
    pub metadata: Vec<ObjectMetadata>,
}
impl Index {
    pub fn lookup(&self, _page: &PageId) -> Result<Option<RecordLocation>> {
        pending("index.lookup")
    }
    pub fn publish(&self, _page: PageId, _location: RecordLocation) -> Result<()> {
        pending("index.publish")
    }
    /// Compare the complete mapping before removing, preserving replacement writes.
    pub fn remove_if_matches(&self, _page: &PageId, _location: &RecordLocation) -> Result<()> {
        pending("index.remove")
    }
    pub fn snapshot(&self) -> Result<IndexSnapshot> {
        pending("index.snapshot")
    }
}
#[cfg(test)]
mod tests { /* Duplicate writes, reverse membership, consistent metadata-only entries. */
}
