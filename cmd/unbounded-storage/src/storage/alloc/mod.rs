// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-disk free-page bitmap.
//!
//! The allocator owns a bit per LBA; clear = free, set = in-use.
//! [`Allocator::alloc`] finds a free bit, sets it, returns the LBA.
//! [`Allocator::free`] clears it. We deliberately do not maintain
//! a free list - on shard restart we reconstruct the bitmap by
//! walking the btree leaves (every reachable LBA is in-use), so
//! the bitmap can be rebuilt cheaply and we don't need a persisted
//! free list.

use std::sync::Mutex;

use crate::storage::types::{Error, Lba};

#[cfg(test)]
mod tests;

/// Capacity-bounded bitmap allocator. Thread-safe through a single
/// mutex; the storage engine drives one allocator per disk and
/// allocation cost is dominated by I/O anyway, so contention is
/// not a concern.
pub struct Allocator {
    inner: Mutex<Inner>,
    capacity: u64,
}

struct Inner {
    /// One bit per LBA; bit-`i` of `words[i/64]` is set iff that
    /// LBA is in use.
    words: Vec<u64>,
    /// Hint for where to start the next scan. Advanced on alloc so
    /// we don't repeatedly re-scan from the start when most of the
    /// low LBAs are taken.
    next: u64,
    /// Count of bits currently set; cheap to maintain and lets
    /// `is_full` / `used_pages` answer without scanning.
    used: u64,
}

impl Allocator {
    pub fn new(capacity_pages: u64) -> Self {
        let n_words = capacity_pages.div_ceil(64) as usize;
        Self {
            inner: Mutex::new(Inner {
                words: vec![0u64; n_words],
                next: 0,
                used: 0,
            }),
            capacity: capacity_pages,
        }
    }

    pub fn capacity(&self) -> u64 {
        self.capacity
    }

    pub fn used_pages(&self) -> u64 {
        self.inner.lock().expect("alloc mutex").used
    }

    pub fn free_pages(&self) -> u64 {
        self.capacity - self.used_pages()
    }

    /// Reserve a single LBA. Returns `OutOfSpace` when the disk is
    /// full. Scans from a rolling hint to amortize the search
    /// cost.
    pub fn alloc(&self) -> Result<Lba, Error> {
        let mut g = self.inner.lock().expect("alloc mutex");
        if g.used >= self.capacity {
            return Err(Error::OutOfSpace);
        }
        // Try the hint forward, then wrap once.
        let start = g.next;
        if let Some(lba) = scan_from(&mut g, start, self.capacity) {
            return Ok(Lba(lba));
        }
        if let Some(lba) = scan_from(&mut g, 0, start) {
            return Ok(Lba(lba));
        }
        Err(Error::OutOfSpace)
    }

    /// Mark `lba` free. Returns `OutOfRange` if `lba` is past the
    /// configured capacity. Idempotent: freeing an already-free
    /// LBA is a logic bug we surface via a debug_assert but the
    /// release-build behaviour is a no-op so we don't corrupt the
    /// `used` count.
    pub fn free(&self, lba: Lba) -> Result<(), Error> {
        if lba.0 >= self.capacity {
            return Err(Error::OutOfRange);
        }
        let (w, b) = word_bit(lba.0);
        let mut g = self.inner.lock().expect("alloc mutex");
        let mask = 1u64 << b;
        if g.words[w] & mask == 0 {
            debug_assert!(false, "free of already-free lba {:?}", lba);
            return Ok(());
        }
        g.words[w] &= !mask;
        g.used -= 1;
        // Keep the hint at the now-free slot so it gets picked
        // again on the next alloc; this stabilises locality and
        // helps the io_uring write coalescer.
        g.next = lba.0;
        Ok(())
    }

    /// Mark `lba` in-use *without* a corresponding [`Allocator::alloc`].
    /// Used during btree-driven rebuild on restart: as we walk
    /// every reachable leaf entry we replay its LBA into the
    /// bitmap so subsequent allocations don't collide.
    pub fn mark_in_use(&self, lba: Lba) -> Result<(), Error> {
        if lba.0 >= self.capacity {
            return Err(Error::OutOfRange);
        }
        let (w, b) = word_bit(lba.0);
        let mut g = self.inner.lock().expect("alloc mutex");
        let mask = 1u64 << b;
        if g.words[w] & mask == 0 {
            g.words[w] |= mask;
            g.used += 1;
        }
        Ok(())
    }

    pub fn is_in_use(&self, lba: Lba) -> Result<bool, Error> {
        if lba.0 >= self.capacity {
            return Err(Error::OutOfRange);
        }
        let (w, b) = word_bit(lba.0);
        let g = self.inner.lock().expect("alloc mutex");
        Ok(g.words[w] & (1u64 << b) != 0)
    }
}

fn word_bit(lba: u64) -> (usize, u32) {
    ((lba / 64) as usize, (lba % 64) as u32)
}

/// Scans `[start, end)` looking for the first cleared bit. Returns
/// the LBA on success and updates the allocator's `next` hint /
/// `used` count.
fn scan_from(inner: &mut Inner, start: u64, end: u64) -> Option<u64> {
    if start >= end {
        return None;
    }
    let mut lba = start;
    let mut w = (lba / 64) as usize;
    let last_word = ((end - 1) / 64) as usize;
    while w <= last_word {
        let word = inner.words[w];
        if word != u64::MAX {
            // At least one zero bit in this word; find the lowest
            // free bit that's also within [start, end).
            let inverted = !word;
            // `trailing_zeros` finds the lowest set bit; mask off
            // anything before `lba`'s bit-offset within the word.
            let bit_offset = if (lba / 64) as usize == w {
                lba % 64
            } else {
                0
            };
            let masked = inverted & (!0u64 << bit_offset);
            if masked != 0 {
                let b = masked.trailing_zeros() as u64;
                let candidate = (w as u64) * 64 + b;
                if candidate < end {
                    inner.words[w] |= 1u64 << b;
                    inner.used += 1;
                    inner.next = candidate + 1;
                    return Some(candidate);
                }
            }
        }
        w += 1;
        lba = (w as u64) * 64;
    }
    None
}
