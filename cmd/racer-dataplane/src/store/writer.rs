//! Bounded dirty queue: reserved -> pending -> writing -> indexed or discarded.
//!
//! Publish plaintext before persistence; retain pending original ciphertext for
//! peer readers. A failed write may discard its dirty copy. Publish index entries
//! only after the complete padded record succeeds; never persist request context.
use super::{index::Index, segment::Segments, slab::Slabs};
use crate::{
    error::{Operation, Result, deferred, pending},
    memory::page::CiphertextCopy,
    runtime::{admission::Reservation, deadline::RequestScope},
};
use std::rc::Rc;
pub struct StoreWriter {
    index: Rc<Index>,
    segments: Rc<Segments>,
    slabs: Rc<Slabs>,
}
pub struct DirtyTicket {
    id: u64,
}
impl StoreWriter {
    pub fn new(index: Rc<Index>, segments: Rc<Segments>, slabs: Rc<Slabs>) -> Self {
        Self {
            index,
            segments,
            slabs,
        }
    }
    /// Retain the descriptor with pending ciphertext. Index publication commits
    /// location and descriptor together only after the entire record completes.
    pub fn enqueue(&self, _page: CiphertextCopy, _dirty: Reservation) -> Result<DirtyTicket> {
        pending("store.enqueue")
    }
    pub fn copy_only(
        &self,
        _page: &crate::model::identity::PageId,
    ) -> Result<Option<CiphertextCopy>> {
        pending("store.pending_copy")
    }
    pub fn metadata(
        &self,
        _version: &crate::model::identity::ObjectVersion,
    ) -> Result<Option<crate::model::metadata::VersionMetadata>> {
        pending("store.pending_metadata")
    }
    pub fn drain<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("store.drain")
    }
}
#[cfg(test)]
mod tests { /* Failed write discard, late-key retirement, dirty limits, index ordering. */
}
