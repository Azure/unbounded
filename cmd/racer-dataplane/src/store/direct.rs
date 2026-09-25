//! Filesystem-discovered O_DIRECT alignment and completion-owned aligned buffers.
//!
//! Check memory, offset, and length constraints from the opened filesystem/file
//! (for example STATX_DIOALIGN where supported). Never assume sector/page size or
//! silently retry buffered I/O. Allocate at the required address alignment; Vec<u8>
//! alone is not sufficient. Zero all padding before writes to avoid leaking memory.
use crate::{
    error::{Result, pending},
    runtime::{admission::Reservation, reactor::IoBuffer},
};
use std::{alloc::Layout, ptr::NonNull};

#[derive(Clone, Copy, Debug)]
pub struct DirectAlignment {
    memory: usize,
    offset: u64,
    length: usize,
}
pub struct AlignedBuffer {
    allocation: NonNull<u8>,
    layout: Layout,
    length: usize,
    reservation: Reservation,
}
/// Only validated aligned extents reach slab positional I/O. Includes padding.
#[derive(Clone, Copy, Debug)]
pub struct DirectExtent {
    offset: u64,
    length: usize,
}
impl DirectAlignment {
    pub fn validate(_memory: usize, _offset: u64, _length: usize) -> Result<Self> {
        pending("direct.alignment")
    }
    pub fn extent(&self, _offset: u64, _logical_length: usize) -> Result<DirectExtent> {
        pending("direct.extent")
    }
    pub fn allocate(&self, _length: usize, _reservation: Reservation) -> Result<AlignedBuffer> {
        pending("direct.allocate")
    }
}
// The allocation constructor is fail-closed until a correct allocator, Drop, and
// completion-lifetime implementation exist. No placeholder unsafe dereference.
impl IoBuffer for AlignedBuffer {
    fn bytes(&self) -> Result<&[u8]> {
        pending("direct.bytes")
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        pending("direct.bytes_mut")
    }
}
#[cfg(test)]
mod tests {
    // Address/offset/length alignment, padding zeroing, overflow, and CQE lifetime.
    // Real Linux tests assert O_DIRECT on opened slabs and no buffered fallback.
}
