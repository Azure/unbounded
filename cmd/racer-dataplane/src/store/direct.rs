//! Discovered direct-I/O geometry and exclusively owned, quota-charged buffers.
use crate::{
    error::{Error, Result},
    runtime::{admission::Reservation, reactor::IoBuffer},
};
use std::{
    alloc::{Layout, alloc_zeroed, dealloc},
    ptr::NonNull,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
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
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DirectExtent {
    offset: u64,
    length: usize,
}
impl DirectExtent {
    pub fn checked(offset: u64, length: usize) -> Result<Self> {
        if length == 0 || offset.checked_add(length as u64).is_none() {
            return Err(Error::CorruptRecord);
        }
        Ok(Self { offset, length })
    }
    pub fn offset(self) -> u64 {
        self.offset
    }
    pub fn length(self) -> usize {
        self.length
    }
}
impl DirectAlignment {
    pub fn validate(memory: usize, offset: u64, length: usize) -> Result<Self> {
        if !memory.is_power_of_two() || offset == 0 || length == 0 || memory > isize::MAX as usize {
            return Err(Error::DirectIoUnsupported);
        }
        Ok(Self {
            memory,
            offset,
            length,
        })
    }
    pub fn memory(self) -> usize {
        self.memory
    }
    pub fn offset(self) -> u64 {
        self.offset
    }
    pub fn length(self) -> usize {
        self.length
    }
    pub fn extent(&self, offset: u64, logical_length: usize) -> Result<DirectExtent> {
        if !offset.is_multiple_of(self.offset) || logical_length == 0 {
            return Err(Error::InvalidConfiguration);
        }
        let offset_unit = usize::try_from(self.offset).map_err(|_| Error::InvalidConfiguration)?;
        let mut a = offset_unit;
        let mut b = self.length;
        while b != 0 {
            let r = a % b;
            a = b;
            b = r;
        }
        let unit = (offset_unit / a)
            .checked_mul(self.length)
            .ok_or(Error::InvalidConfiguration)?;
        let length = logical_length
            .checked_add(unit - 1)
            .and_then(|v| (v / unit).checked_mul(unit))
            .ok_or(Error::InvalidConfiguration)?;
        DirectExtent::checked(offset, length)
    }
    pub fn allocate(&self, length: usize, reservation: Reservation) -> Result<AlignedBuffer> {
        if length == 0 || !length.is_multiple_of(self.length) || reservation.amount() < length {
            return Err(Error::InvalidConfiguration);
        }
        let layout = Layout::from_size_align(length, self.memory)
            .map_err(|_| Error::InvalidConfiguration)?;
        // SAFETY: valid nonzero layout; ownership is unique and Drop uses this layout.
        let allocation = NonNull::new(unsafe { alloc_zeroed(layout) }).ok_or(Error::Overloaded)?;
        Ok(AlignedBuffer {
            allocation,
            layout,
            length,
            reservation,
        })
    }
    pub fn check(&self, extent: DirectExtent, buffer: &AlignedBuffer) -> Result<()> {
        if !(buffer.allocation.as_ptr() as usize).is_multiple_of(self.memory)
            || !extent.offset.is_multiple_of(self.offset)
            || !extent.length.is_multiple_of(self.length)
            || buffer.length != extent.length
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
impl AlignedBuffer {
    pub fn len(&self) -> usize {
        self.length
    }
    pub fn is_empty(&self) -> bool {
        self.length == 0
    }
}
impl Drop for AlignedBuffer {
    fn drop(&mut self) {
        // SAFETY: allocation is exclusively owned, with its original layout.
        unsafe { dealloc(self.allocation.as_ptr(), self.layout) };
    }
}
impl crate::runtime::reactor::sealed::Sealed for AlignedBuffer {}
impl IoBuffer for AlignedBuffer {
    fn bytes(&self) -> Result<&[u8]> {
        // SAFETY: initialized allocation remains live for this borrow.
        Ok(unsafe { std::slice::from_raw_parts(self.allocation.as_ptr(), self.length) })
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        // SAFETY: exclusive borrow prevents mutable aliases.
        Ok(unsafe { std::slice::from_raw_parts_mut(self.allocation.as_ptr(), self.length) })
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn geometry_rounds_without_assuming_page_size() {
        let a = DirectAlignment::validate(512, 512, 1024).unwrap();
        assert_eq!(a.extent(512, 1025).unwrap().length(), 2048);
        assert!(a.extent(1, 1).is_err());
        assert!(a.extent(0, usize::MAX).is_err());
        assert!(DirectAlignment::validate(3, 512, 512).is_err());
        assert!(DirectExtent::checked(u64::MAX, 1).is_err());
    }
}
