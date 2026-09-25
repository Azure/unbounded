//! Per-worker second-chance segment clock; no copying or compaction.
use super::{
    index::Index,
    segment::{SegmentId, Segments},
};
use crate::error::{Operation, Result, deferred, pending};
use std::rc::Rc;
pub struct SegmentClock {
    index: Rc<Index>,
    segments: Rc<Segments>,
    free_reserve: usize,
}
impl SegmentClock {
    pub fn new(index: Rc<Index>, segments: Rc<Segments>, free_reserve: usize) -> Self {
        Self {
            index,
            segments,
            free_reserve,
        }
    }
    pub fn mark_read(&self, _segment: SegmentId) -> Result<()> {
        pending("eviction.mark")
    }
    pub fn reclaim(&self) -> Operation<'_, ()> {
        deferred("eviction.reclaim")
    }
}
#[cfg(test)]
mod tests { /* Clear/skip once, free reserve, active leases, compare-and-remove maps. */
}
