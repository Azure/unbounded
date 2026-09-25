//! ext4 slab files opened with O_DIRECT and bounded asynchronous positional I/O.
//!
//! Alignment applies to allocation base, file offsets, and submitted lengths.
//! Geometry must fit a padded full page and header without crossing segments.
//! Partial completions are failures unless a remaining aligned operation is valid.
//! Never scan/zero payloads at startup or claim fsync durability.
use super::direct::{AlignedBuffer, DirectAlignment, DirectExtent};
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct SlabId(pub u64);
#[derive(Clone, Copy, Debug)]
pub struct SlabLocation {
    pub slab: SlabId,
    pub extent: DirectExtent,
}
use crate::{
    error::{Operation, deferred},
    runtime::{deadline::RequestScope, reactor::Reactor},
};
use std::{path::PathBuf, rc::Rc};
pub struct Slabs {
    worker: crate::model::identity::WorkerId,
    directory: PathBuf,
    reactor: Rc<Reactor>,
    slab_bytes: u64,
    segment_bytes: u64,
}
impl Slabs {
    pub fn new(
        worker: crate::model::identity::WorkerId,
        directory: PathBuf,
        reactor: Rc<Reactor>,
        slab_bytes: u64,
        segment_bytes: u64,
    ) -> Self {
        Self {
            worker,
            directory,
            reactor,
            slab_bytes,
            segment_bytes,
        }
    }
    pub fn open(&self) -> Operation<'_, DirectAlignment> {
        deferred("slab.open_direct")
    }
    pub fn read<'a>(
        &'a self,
        _location: SlabLocation,
        _buffer: AlignedBuffer,
        _scope: &'a RequestScope,
    ) -> Operation<'a, AlignedBuffer> {
        deferred("slab.read_direct")
    }
    pub fn write<'a>(
        &'a self,
        _location: SlabLocation,
        _buffer: AlignedBuffer,
        _scope: &'a RequestScope,
    ) -> Operation<'a, AlignedBuffer> {
        deferred("slab.write_direct")
    }
}
#[cfg(test)]
mod tests { /* Unsupported O_DIRECT, actual filesystem alignment, short I/O, geometry. */
}
