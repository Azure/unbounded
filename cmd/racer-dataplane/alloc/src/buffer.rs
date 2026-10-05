use crate::{Error, Result};
use std::{
    alloc::{Layout, alloc_zeroed, dealloc},
    cell::RefCell,
    fmt,
    ptr::NonNull,
    rc::{Rc, Weak},
};
use uring_runtime::reactor::IoBuffer;
use zeroize::Zeroize;

/// Caller-owned accounting retained while memory is live, including idle pooling.
pub trait Charge: 'static {
    /// Whether this guard accounts for at least `bytes` live allocation bytes.
    fn covers(&self, bytes: usize) -> bool;
}
impl Charge for () {
    fn covers(&self, _bytes: usize) -> bool {
        true
    }
}

/// Direct-I/O alignment requirements. Offset and length units may be any positive
/// integers; only the memory alignment must be a power of two.
#[must_use]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Alignment {
    memory: usize,
    offset: u64,
    length: usize,
}
/// A nonempty, non-overflowing file range for one bounded I/O transfer.
#[must_use]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Extent {
    offset: u64,
    length: usize,
}
impl Extent {
    /// Reject empty ranges, offset overflow, and lengths above the transfer cap.
    pub fn new(offset: u64, length: usize) -> Result<Self> {
        if length == 0
            || length > Alignment::MAX_TRANSFER_LENGTH
            || offset.checked_add(length as u64).is_none()
        {
            return Err(Error::Corrupt);
        }
        Ok(Self { offset, length })
    }
    /// Starting byte offset in the file.
    #[must_use]
    pub fn offset(self) -> u64 {
        self.offset
    }
    /// Transfer length in bytes, including any padding.
    #[must_use]
    pub fn length(self) -> usize {
        self.length
    }
}
impl Alignment {
    /// Conservative single-transfer limit of 1 GiB.
    ///
    /// This fits the runtime's `u32` length and stays below Linux's
    /// `MAX_RW_COUNT` (`i32::MAX` rounded down to a base-page boundary) on
    /// supported Linux base-page sizes, without querying host state. Keeping a
    /// fixed conservative bound also makes geometry checks deterministic under
    /// simulation and Miri. Larger records must be split by the caller.
    pub const MAX_TRANSFER_LENGTH: usize = 1 << 30;

    /// Validate alignment units without requiring offset/length powers of two.
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
    /// Required memory address alignment.
    #[must_use]
    pub fn memory(self) -> usize {
        self.memory
    }
    /// Required file offset unit.
    #[must_use]
    pub fn offset(self) -> u64 {
        self.offset
    }
    /// Required transfer length unit.
    #[must_use]
    pub fn length(self) -> usize {
        self.length
    }
    /// Round up to the least common multiple of offset and length units so the
    /// next appended extent is also offset-aligned. Reject overflow and lengths
    /// above [`Self::MAX_TRANSFER_LENGTH`] before allocating memory.
    pub fn extent(&self, offset: u64, logical: usize) -> Result<Extent> {
        if !offset.is_multiple_of(self.offset)
            || logical == 0
            || logical > Self::MAX_TRANSFER_LENGTH
        {
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
        if length > Self::MAX_TRANSFER_LENGTH {
            return Err(Error::InvalidConfiguration);
        }
        Extent::new(offset, length)
    }
    /// Allocate zeroed stable storage and retain its primary accounting guard.
    /// Length must be nonzero, length-aligned, covered by `charge`, and no larger
    /// than [`Self::MAX_TRANSFER_LENGTH`]. Allocation failure returns `Busy`.
    pub fn allocate<C: Charge>(&self, length: usize, charge: C) -> Result<AlignedBuffer<C>> {
        if length == 0
            || length > Self::MAX_TRANSFER_LENGTH
            || !length.is_multiple_of(self.length)
            || !charge.covers(length)
        {
            return Err(Error::InvalidConfiguration);
        }
        let layout = Layout::from_size_align(length, self.memory)
            .map_err(|_| Error::InvalidConfiguration)?;
        // SAFETY: valid nonzero layout; Allocation owns the matching deallocation.
        let pointer = NonNull::new(unsafe { alloc_zeroed(layout) }).ok_or(Error::Busy)?;
        Ok(AlignedBuffer {
            allocation: Some(Allocation {
                pointer,
                layout,
                charge,
                retained: Vec::new(),
            }),
            pool: Weak::new(),
        })
    }
    /// Validate the address, file offset, transfer unit, and exact buffer length.
    pub fn check<C: Charge>(&self, extent: Extent, buffer: &AlignedBuffer<C>) -> Result<()> {
        if !(buffer.allocation().pointer.as_ptr() as usize).is_multiple_of(self.memory)
            || !extent.offset.is_multiple_of(self.offset)
            || !extent.length.is_multiple_of(self.length)
            || buffer.len() != extent.length
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}

// Owns both the raw allocation and its accounting. Moving this value transfers
// ownership; no charge sentinel or copied raw owner is needed for pooling.
struct Allocation<C: Charge> {
    pointer: NonNull<u8>,
    layout: Layout,
    charge: C,
    retained: Vec<Rc<C>>,
}
impl<C: Charge> Allocation<C> {
    fn as_mut_slice(&mut self) -> &mut [u8] {
        // SAFETY: exclusive owner, initialized nonzero allocation, original layout.
        unsafe { std::slice::from_raw_parts_mut(self.pointer.as_ptr(), self.layout.size()) }
    }
}
impl<C: Charge> Drop for Allocation<C> {
    fn drop(&mut self) {
        self.as_mut_slice().zeroize();
        // SAFETY: this owner holds the allocation and its original layout. Guards
        // are dropped only after zeroization and deallocation complete.
        unsafe { dealloc(self.pointer.as_ptr(), self.layout) };
    }
}

/// Worker-local, exclusively owned, initialized storage with a stable address.
///
/// Moving the buffer never moves its bytes. Drop zeroizes the bytes before
/// freeing or pooling them. Idle storage retains its primary charge, but not
/// additional guards attached with [`Self::retain`].
#[must_use]
pub struct AlignedBuffer<C: Charge> {
    // Always Some while publicly accessible; taken only by Drop for pool transfer.
    allocation: Option<Allocation<C>>,
    pool: Weak<RefCell<Option<Self>>>,
}
impl<C: Charge> fmt::Debug for AlignedBuffer<C> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("AlignedBuffer")
            .field("length", &self.len())
            .field("alignment", &self.allocation().layout.align())
            .field("retained", &self.allocation().retained.len())
            .finish_non_exhaustive()
    }
}
impl<C: Charge> AlignedBuffer<C> {
    fn allocation(&self) -> &Allocation<C> {
        self.allocation.as_ref().expect("live buffer allocation")
    }
    fn allocation_mut(&mut self) -> &mut Allocation<C> {
        self.allocation.as_mut().expect("live buffer allocation")
    }
    pub(crate) fn pooled(mut self, pool: &Rc<RefCell<Option<Self>>>) -> Self {
        self.pool = Rc::downgrade(pool);
        self
    }
    pub(crate) fn rebind(&mut self, charge: C) -> Result<()> {
        if !charge.covers(self.len()) {
            return Err(Error::InvalidConfiguration);
        }
        self.allocation_mut().charge = charge;
        Ok(())
    }
    /// Retain additional accounting through the final kernel completion.
    pub fn retain(&mut self, charge: Rc<C>) {
        self.allocation_mut().retained.push(charge);
    }
    /// Initialized allocation length, including padding.
    #[must_use]
    pub fn len(&self) -> usize {
        self.allocation().layout.size()
    }
    /// Always false: construction rejects zero-length buffers.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        false
    }
    /// Borrow all initialized bytes without moving or resizing the allocation.
    #[must_use]
    pub fn as_slice(&self) -> &[u8] {
        // SAFETY: initialized allocation remains live throughout this borrow.
        unsafe { std::slice::from_raw_parts(self.allocation().pointer.as_ptr(), self.len()) }
    }
    /// Exclusively borrow all initialized bytes.
    #[must_use]
    pub fn as_mut_slice(&mut self) -> &mut [u8] {
        self.allocation_mut().as_mut_slice()
    }
    /// Compatibility accessor matching [`IoBuffer`]; this always succeeds.
    pub fn bytes(&self) -> Result<&[u8]> {
        Ok(self.as_slice())
    }
    /// Compatibility accessor matching [`IoBuffer`]; this always succeeds.
    pub fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(self.as_mut_slice())
    }
}
impl<C: Charge> Drop for AlignedBuffer<C> {
    fn drop(&mut self) {
        if let Some(pool) = self.pool.upgrade() {
            self.allocation_mut().as_mut_slice().zeroize();
            // Caller-owned guard destructors may access the pool. Do not invoke
            // them while holding its RefCell borrow. Allocation remains owned
            // by this buffer if a destructor unwinds.
            self.allocation_mut().retained.clear();
            if let Ok(mut idle) = pool.try_borrow_mut()
                && idle.is_none()
            {
                *idle = Some(Self {
                    allocation: self.allocation.take(),
                    pool: Weak::new(),
                });
            }
        }
        // Otherwise field drop zeroizes and frees the allocation, even when the
        // pool is gone, occupied, or borrowed. Idle buffers never repool themselves.
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

#[cfg(test)]
mod tests;
