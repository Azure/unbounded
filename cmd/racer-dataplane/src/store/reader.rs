//! Atomically lease indexed locations, read padded records, validate identity/generation.
//!
//! This layer returns opaque ciphertext. AEAD belongs to the fill worker; corruption
//! invalidates only the matching mapping and becomes a miss, never plaintext.
use super::{index::Index, segment::Segments, slab::Slabs};
use crate::{
    error::{Operation, deferred},
    memory::{page::CiphertextCopy, pool::BufferPool},
    model::identity::PageId,
    runtime::deadline::RequestScope,
};
use std::rc::Rc;
pub struct StoreReader {
    clock: Rc<super::eviction::SegmentClock>,
    index: Rc<Index>,
    segments: Rc<Segments>,
    slabs: Rc<Slabs>,
    buffers: Rc<BufferPool>,
}
impl StoreReader {
    /// Descriptor lookup without slab I/O, including retained nonzero-page entries.
    pub fn metadata(
        &self,
        version: &crate::model::identity::ObjectVersion,
    ) -> crate::error::Result<Option<crate::model::metadata::VersionMetadata>> {
        self.index.version(version)
    }
    pub fn new(
        clock: Rc<super::eviction::SegmentClock>,
        index: Rc<Index>,
        segments: Rc<Segments>,
        slabs: Rc<Slabs>,
        buffers: Rc<BufferPool>,
    ) -> Self {
        Self {
            clock,
            index,
            segments,
            slabs,
            buffers,
        }
    }
    pub fn read<'a>(
        &'a self,
        _page: &'a PageId,
        _scope: &'a RequestScope,
    ) -> Operation<'a, Option<CiphertextCopy>> {
        deferred("store.read")
    }
}
#[cfg(test)]
mod tests { /* Stale/torn headers, expected generation, key absence, completion races. */
}
