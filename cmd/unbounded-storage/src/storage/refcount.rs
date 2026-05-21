// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-disk refcount + clock bit table.
//!
//! Each LBA on a disk maps to one [`AtomicU32`]. The layout is:
//!
//! ```text
//!   31           30 ............ 0
//!   +-----------+----------------+
//!   | referenced|   pin count    |
//!   +-----------+----------------+
//! ```
//!
//! - **pin count** (low 31 bits): incremented when a reader or
//!   writer is actively touching the page; the eviction policy is
//!   forbidden from reclaiming a page while this is non-zero.
//! - **referenced** (bit 31): the "clock" / "second chance" bit
//!   used by the SIEVE / S3-FIFO eviction policy. Set on every
//!   hit, cleared by the background sweep.
//!
//! Co-locating the two avoids a second atomic word per LBA and lets
//! the eviction worker read both fields in a single load.

use std::sync::atomic::{AtomicU32, Ordering};

use crate::storage::types::Error;

const REFERENCED_BIT: u32 = 1 << 31;
const PIN_MASK: u32 = !REFERENCED_BIT;

/// Owning table of refcount words, indexed by LBA. Capacity is
/// fixed at construction; callers that need a growable
/// representation can wrap this type.
pub struct RefcountTable {
    slots: Box<[AtomicU32]>,
}

impl RefcountTable {
    pub fn new(capacity_pages: u64) -> Self {
        // The cast is safe: callers that pass more than `usize::MAX`
        // pages have bigger problems than the integer width.
        let cap = capacity_pages as usize;
        let mut v = Vec::with_capacity(cap);
        v.resize_with(cap, || AtomicU32::new(0));
        Self {
            slots: v.into_boxed_slice(),
        }
    }

    pub fn capacity(&self) -> u64 {
        self.slots.len() as u64
    }

    fn slot(&self, lba: u64) -> Result<&AtomicU32, Error> {
        self.slots.get(lba as usize).ok_or(Error::OutOfRange)
    }

    /// Increment the pin counter; returns a [`PinGuard`] that
    /// decrements on drop. Returns `Err(OutOfRange)` if `lba` is
    /// outside the table.
    pub fn pin(&self, lba: u64) -> Result<PinGuard<'_>, Error> {
        let slot = self.slot(lba)?;
        // Loop avoids ever incrementing past the 31-bit pin field
        // into the referenced bit. In practice 2^31 concurrent
        // holders is impossible, but the bit is sacred and we
        // shouldn't ever trample it.
        loop {
            let cur = slot.load(Ordering::Acquire);
            let pins = cur & PIN_MASK;
            if pins == PIN_MASK {
                // Pin count saturated; refuse to wrap. The caller
                // can treat this as a transient OutOfSpace-style
                // failure.
                return Err(Error::OutOfSpace);
            }
            let next = (cur & REFERENCED_BIT) | (pins + 1);
            if slot
                .compare_exchange_weak(cur, next, Ordering::AcqRel, Ordering::Acquire)
                .is_ok()
            {
                return Ok(PinGuard { table: self, lba });
            }
        }
    }

    fn unpin_raw(&self, lba: u64) {
        // Unconditional decrement is fine because PinGuards only
        // exist for slots we previously CAS-incremented.
        let slot = match self.slot(lba) {
            Ok(s) => s,
            // OutOfRange shouldn't happen because we only construct
            // PinGuards from valid slots, but a stray call would be
            // a logic bug, not an unrecoverable one.
            Err(_) => return,
        };
        // We use a CAS loop instead of `fetch_sub(1)` to avoid ever
        // touching the referenced bit.
        loop {
            let cur = slot.load(Ordering::Acquire);
            let pins = cur & PIN_MASK;
            if pins == 0 {
                debug_assert!(false, "unpin of slot with zero pins");
                return;
            }
            let next = (cur & REFERENCED_BIT) | (pins - 1);
            if slot
                .compare_exchange_weak(cur, next, Ordering::AcqRel, Ordering::Acquire)
                .is_ok()
            {
                return;
            }
        }
    }

    /// Set the clock bit. Called on every cache hit.
    pub fn mark_referenced(&self, lba: u64) -> Result<(), Error> {
        let slot = self.slot(lba)?;
        slot.fetch_or(REFERENCED_BIT, Ordering::Release);
        Ok(())
    }

    /// Clear the clock bit. Called by the eviction sweeper as it
    /// walks the ring.
    pub fn clear_referenced(&self, lba: u64) -> Result<(), Error> {
        let slot = self.slot(lba)?;
        slot.fetch_and(PIN_MASK, Ordering::Release);
        Ok(())
    }

    pub fn is_referenced(&self, lba: u64) -> Result<bool, Error> {
        let slot = self.slot(lba)?;
        Ok(slot.load(Ordering::Acquire) & REFERENCED_BIT != 0)
    }

    pub fn pin_count(&self, lba: u64) -> Result<u32, Error> {
        let slot = self.slot(lba)?;
        Ok(slot.load(Ordering::Acquire) & PIN_MASK)
    }

    pub fn is_pinned(&self, lba: u64) -> Result<bool, Error> {
        Ok(self.pin_count(lba)? > 0)
    }

    /// Reset both fields, used when the eviction worker frees a
    /// page and the LBA goes back to the allocator. The caller
    /// must hold an exclusive view of the slot (eviction worker is
    /// the only writer in production).
    pub fn reset(&self, lba: u64) -> Result<(), Error> {
        let slot = self.slot(lba)?;
        slot.store(0, Ordering::Release);
        Ok(())
    }
}

/// RAII pin handle. Held by readers / writers for the duration of
/// the I/O so the eviction policy can't reclaim the LBA underneath
/// them.
pub struct PinGuard<'t> {
    table: &'t RefcountTable,
    lba: u64,
}

impl<'t> PinGuard<'t> {
    pub fn lba(&self) -> u64 {
        self.lba
    }
}

impl<'t> Drop for PinGuard<'t> {
    fn drop(&mut self) {
        self.table.unpin_raw(self.lba);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pin_guard_increments_and_decrements() {
        let t = RefcountTable::new(8);
        assert_eq!(t.pin_count(3).unwrap(), 0);
        {
            let _g = t.pin(3).unwrap();
            assert_eq!(t.pin_count(3).unwrap(), 1);
            let _g2 = t.pin(3).unwrap();
            assert_eq!(t.pin_count(3).unwrap(), 2);
            assert!(t.is_pinned(3).unwrap());
        }
        assert_eq!(t.pin_count(3).unwrap(), 0);
        assert!(!t.is_pinned(3).unwrap());
    }

    #[test]
    fn referenced_bit_is_independent_of_pin_count() {
        let t = RefcountTable::new(2);
        let _g = t.pin(0).unwrap();
        t.mark_referenced(0).unwrap();
        assert!(t.is_referenced(0).unwrap());
        assert_eq!(t.pin_count(0).unwrap(), 1);
        t.clear_referenced(0).unwrap();
        assert!(!t.is_referenced(0).unwrap());
        assert_eq!(t.pin_count(0).unwrap(), 1);
    }

    #[test]
    fn out_of_range_returns_error() {
        let t = RefcountTable::new(2);
        assert!(matches!(t.pin(5), Err(Error::OutOfRange)));
        assert!(matches!(t.mark_referenced(2), Err(Error::OutOfRange)));
        assert!(matches!(t.is_referenced(2), Err(Error::OutOfRange)));
    }

    #[test]
    fn reset_clears_both_fields() {
        let t = RefcountTable::new(2);
        let g = t.pin(1).unwrap();
        t.mark_referenced(1).unwrap();
        drop(g);
        t.reset(1).unwrap();
        assert!(!t.is_referenced(1).unwrap());
        assert_eq!(t.pin_count(1).unwrap(), 0);
    }
}
