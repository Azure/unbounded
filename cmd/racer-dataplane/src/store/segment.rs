//! Append/seal/evict/reuse state machine with non-wrapping generations and I/O leases.
use super::slab::SlabLocation;
use crate::{
    error::{Result, pending},
    model::identity::WorkerId,
};
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct SegmentId(pub u64);
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct Generation(pub u64);
#[derive(Clone, Copy, Debug)]
pub enum SegmentState {
    Free,
    Open,
    Sealed,
    Evicting,
}
pub struct SegmentSnapshot {
    pub id: SegmentId,
    pub generation: Generation,
    pub state: SegmentState,
    pub used_bytes: u64,
}
pub struct SegmentLease {
    pub(crate) id: SegmentId,
    pub(crate) generation: Generation,
}
pub struct AppendLease {
    pub segment: SegmentLease,
    pub location: SlabLocation,
}
pub struct Segments {
    worker: WorkerId,
    segment_bytes: u64,
}
impl Segments {
    pub fn new(worker: WorkerId, segment_bytes: u64) -> Self {
        Self {
            worker,
            segment_bytes,
        }
    }
    /// Reserve a padded, aligned, whole record; seal insufficient tails.
    pub fn append(&self, _disk_bytes: usize) -> Result<AppendLease> {
        pending("segment.append")
    }
    pub fn lease(&self, _id: SegmentId, _generation: Generation) -> Result<SegmentLease> {
        pending("segment.lease")
    }
    pub fn recycle(&self, _id: SegmentId) -> Result<()> {
        pending("segment.recycle")
    }
}
#[cfg(test)]
mod tests { /* ABA, generation overflow, aligned tails, no reuse before I/O fences. */
}
