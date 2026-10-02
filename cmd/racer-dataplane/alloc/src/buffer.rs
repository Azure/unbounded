use crate::{Error, Result};
use std::{
    alloc::{Layout, alloc_zeroed, dealloc},
    cell::RefCell,
    ptr::NonNull,
    rc::{Rc, Weak},
};
use uring_runtime::reactor::IoBuffer;
use zeroize::Zeroize;

/// Caller-owned accounting retained while memory is live, including idle pooling.
pub trait Charge: 'static {
    fn covers(&self, bytes: usize) -> bool;
}
impl Charge for () {
    fn covers(&self, _bytes: usize) -> bool {
        true
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Alignment {
    memory: usize,
    offset: u64,
    length: usize,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Extent {
    offset: u64,
    length: usize,
}
impl Extent {
    pub fn new(offset: u64, length: usize) -> Result<Self> {
        if length == 0 || offset.checked_add(length as u64).is_none() {
            return Err(Error::Corrupt);
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
impl Alignment {
    pub fn new(memory: usize, offset: u64, length: usize) -> Result<Self> {
        if !memory.is_power_of_two() || offset == 0 || length == 0 || memory > isize::MAX as usize {
            return Err(Error::Unsupported);
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
    pub fn extent(&self, offset: u64, logical: usize) -> Result<Extent> {
        if !offset.is_multiple_of(self.offset) || logical == 0 {
            return Err(Error::InvalidConfiguration);
        }
        let offset_unit = usize::try_from(self.offset).map_err(|_| Error::InvalidConfiguration)?;
        let (mut a, mut b) = (offset_unit, self.length);
        while b != 0 {
            (a, b) = (b, a % b);
        }
        let unit = (offset_unit / a)
            .checked_mul(self.length)
            .ok_or(Error::InvalidConfiguration)?;
        let length = logical
            .checked_add(unit - 1)
            .and_then(|v| (v / unit).checked_mul(unit))
            .ok_or(Error::InvalidConfiguration)?;
        Extent::new(offset, length)
    }
    pub fn allocate<C: Charge>(&self, length: usize, charge: C) -> Result<AlignedBuffer<C>> {
        if length == 0 || !length.is_multiple_of(self.length) || !charge.covers(length) {
            return Err(Error::InvalidConfiguration);
        }
        let layout = Layout::from_size_align(length, self.memory)
            .map_err(|_| Error::InvalidConfiguration)?;
        // SAFETY: valid nonzero layout; Drop owns the matching deallocation.
        let allocation = NonNull::new(unsafe { alloc_zeroed(layout) }).ok_or(Error::Busy)?;
        Ok(AlignedBuffer {
            allocation,
            layout,
            length,
            charge: Some(charge),
            retained: Vec::new(),
            pool: Weak::new(),
        })
    }
    pub fn check<C: Charge>(&self, extent: Extent, buffer: &AlignedBuffer<C>) -> Result<()> {
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

pub struct AlignedBuffer<C: Charge> {
    allocation: NonNull<u8>,
    layout: Layout,
    length: usize,
    charge: Option<C>,
    retained: Vec<Rc<C>>,
    pool: Weak<RefCell<Option<Self>>>,
}
impl<C: Charge> AlignedBuffer<C> {
    pub(crate) fn pooled(mut self, pool: &Rc<RefCell<Option<Self>>>) -> Self {
        self.pool = Rc::downgrade(pool);
        self
    }
    pub(crate) fn rebind(&mut self, charge: C) -> Result<()> {
        if !charge.covers(self.length) {
            return Err(Error::InvalidConfiguration);
        }
        self.charge = Some(charge);
        Ok(())
    }
    /// Retain additional accounting through the final kernel completion.
    pub fn retain(&mut self, charge: Rc<C>) {
        self.retained.push(charge);
    }
    pub fn len(&self) -> usize {
        self.length
    }
    pub fn is_empty(&self) -> bool {
        self.length == 0
    }
    pub fn as_slice(&self) -> &[u8] {
        // SAFETY: initialized allocation remains live throughout this borrow.
        unsafe { std::slice::from_raw_parts(self.allocation.as_ptr(), self.length) }
    }
    pub fn as_mut_slice(&mut self) -> &mut [u8] {
        // SAFETY: exclusive borrow prevents mutable aliases.
        unsafe { std::slice::from_raw_parts_mut(self.allocation.as_ptr(), self.length) }
    }
    pub fn bytes(&self) -> Result<&[u8]> {
        Ok(self.as_slice())
    }
    pub fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(self.as_mut_slice())
    }
}
impl<C: Charge> Drop for AlignedBuffer<C> {
    fn drop(&mut self) {
        // A missing charge marks ownership transferred into the pool.
        if self.charge.is_none() {
            return;
        }
        self.as_mut_slice().zeroize();
        self.retained.clear();
        if let Some(pool) = self.pool.upgrade()
            && let Ok(mut idle) = pool.try_borrow_mut()
            && idle.is_none()
        {
            *idle = Some(Self {
                allocation: self.allocation,
                layout: self.layout,
                length: self.length,
                charge: self.charge.take(),
                retained: Vec::new(),
                pool: Weak::new(),
            });
            return;
        }
        // SAFETY: allocation is exclusively owned with its original layout.
        unsafe { dealloc(self.allocation.as_ptr(), self.layout) };
    }
}
// SAFETY: owned aligned backing is initialized, stable, and live until Drop.
unsafe impl<C: Charge> IoBuffer for AlignedBuffer<C> {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        AlignedBuffer::bytes(self)
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        AlignedBuffer::bytes_mut(self)
    }
}
