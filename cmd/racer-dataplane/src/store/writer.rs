//! Bounded dirty queue: reserved -> pending -> writing -> indexed or discarded.
//!
//! Publish plaintext before persistence; retain pending original ciphertext for
//! peer readers. A failed write may discard its dirty copy. Publish index entries
//! only after the complete padded record succeeds; never persist request context.
use super::{index::Index, segment::Segments, slab::Slabs};
use crate::{
    error::{Operation, Result, deferred, pending},
    memory::pool::CiphertextPage,
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
    pub fn enqueue(&self, _page: CiphertextPage, _dirty: Reservation) -> Result<DirtyTicket> {
        pending("store.enqueue")
    }
    pub fn drain<'a>(&'a self, _scope: &'a RequestScope) -> Operation<'a, ()> {
        deferred("store.drain")
    }
}
#[cfg(test)]
mod tests { /* Failed write discard, late-key retirement, dirty limits, index ordering. */
}
