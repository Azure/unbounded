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
    /// Maximum LBA value ever recorded as in-use through this
    /// allocator instance. Monotonic non-decreasing: `free` does
    /// not lower it. Btree recovery uses this to bound its
    /// rebuild-fallback scan.
    max_ever: u64,
}

impl Allocator {
    pub fn new(capacity_pages: u64) -> Self {
        let n_words = capacity_pages.div_ceil(64) as usize;
        Self {
            inner: Mutex::new(Inner {
                words: vec![0u64; n_words],
                next: 0,
                used: 0,
                max_ever: 0,
            }),
            capacity: capacity_pages,
        }
    }

    pub fn capacity(&self) -> u64 {
        self.capacity
    }

    pub fn used_pages(&self) -> u64 {
        self.inner.lock().unwrap().used
    }

    pub fn free_pages(&self) -> u64 {
        self.capacity - self.used_pages()
    }

    /// Reserve a single LBA. Returns `OutOfSpace` when the disk is
    /// full. Scans from a rolling hint to amortize the search
    /// cost.
    pub fn alloc(&self) -> Result<Lba, Error> {
        let mut g = self.inner.lock().unwrap();
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

    /// Reserve `n` contiguous LBAs. Returns the start LBA. A bump-
    /// style scan from the current hint suffices because the
    /// engine writes user pages as soon as it allocates them, so
    /// the high-water region stays mostly contiguous: scanning is
    /// O(`n`) (the requested run length) per allocation on a fresh
    /// disk, with worst case O(`capacity`) under fragmentation when
    /// the hint region is exhausted and the wrap-around scan walks
    /// the full bitmap.
    pub fn alloc_contig(&self, n: u64) -> Result<Lba, Error> {
        if n == 0 {
            return Err(Error::OutOfRange);
        }
        if n == 1 {
            return self.alloc();
        }
        let mut g = self.inner.lock().unwrap();
        if g.used + n > self.capacity {
            return Err(Error::OutOfSpace);
        }
        let start = g.next;
        if let Some(lba) = scan_run(&mut g, start, self.capacity, n) {
            return Ok(Lba(lba));
        }
        if let Some(lba) = scan_run(&mut g, 0, start, n) {
            return Ok(Lba(lba));
        }
        Err(Error::OutOfSpace)
    }

    /// Mark a run of `n` LBAs starting at `start` free. Equivalent
    /// to calling `free` for each LBA in the run but holds the
    /// mutex once and skips the per-LBA debug_assert; callers
    /// guarantee the run was previously allocated as a single
    /// [`Self::alloc_contig`] call.
    pub fn free_range(&self, start: Lba, n: u64) -> Result<(), Error> {
        if n == 0 {
            return Ok(());
        }
        let end = start.0.checked_add(n).ok_or(Error::OutOfRange)?;
        if end > self.capacity {
            return Err(Error::OutOfRange);
        }
        let mut g = self.inner.lock().unwrap();
        for lba in start.0..end {
            let (w, b) = word_bit(lba);
            let mask = 1u64 << b;
            if g.words[w] & mask != 0 {
                g.words[w] &= !mask;
                g.used -= 1;
            }
        }
        g.next = start.0;
        Ok(())
    }

    /// Mark `lba` free. Returns `OutOfRange` if `lba` is past the
    /// configured capacity. Idempotent: freeing an already-free
    /// LBA is a logic bug we surface via a debug_assert but the
    /// release-build behavior is a no-op so we don't corrupt the
    /// `used` count.
    pub fn free(&self, lba: Lba) -> Result<(), Error> {
        if lba.0 >= self.capacity {
            return Err(Error::OutOfRange);
        }
        let (w, b) = word_bit(lba.0);
        let mut g = self.inner.lock().unwrap();
        let mask = 1u64 << b;
        if g.words[w] & mask == 0 {
            debug_assert!(false, "free of already-free lba {:?}", lba);
            return Ok(());
        }
        g.words[w] &= !mask;
        g.used -= 1;
        // Keep the hint at the now-free slot so it gets picked
        // again on the next alloc; this stabilizes locality and
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
        let mut g = self.inner.lock().unwrap();
        let mask = 1u64 << b;
        if g.words[w] & mask == 0 {
            g.words[w] |= mask;
            g.used += 1;
        }
        g.max_ever = g.max_ever.max(lba.0);
        Ok(())
    }

    /// Mark a run of `n` LBAs starting at `start` in-use.
    /// Mirror of [`Self::free_range`]; takes the lock once and
    /// is idempotent per-bit. Used during btree recovery to
    /// replay a contiguous data-page run referenced by a leaf
    /// entry into the bitmap so subsequent allocations cannot
    /// hand out any LBA in the run.
    pub fn mark_range_in_use(&self, start: Lba, n: u64) -> Result<(), Error> {
        if n == 0 {
            return Ok(());
        }
        let end = start.0.checked_add(n).ok_or(Error::OutOfRange)?;
        if end > self.capacity {
            return Err(Error::OutOfRange);
        }
        let mut g = self.inner.lock().unwrap();
        for lba in start.0..end {
            let (w, b) = word_bit(lba);
            let mask = 1u64 << b;
            if g.words[w] & mask == 0 {
                g.words[w] |= mask;
                g.used += 1;
            }
        }
        g.max_ever = g.max_ever.max(end - 1);
        Ok(())
    }

    pub fn is_in_use(&self, lba: Lba) -> Result<bool, Error> {
        if lba.0 >= self.capacity {
            return Err(Error::OutOfRange);
        }
        let (w, b) = word_bit(lba.0);
        let g = self.inner.lock().unwrap();
        Ok(g.words[w] & (1u64 << b) != 0)
    }

    /// Largest LBA that has ever been recorded as in-use through this
    /// allocator instance. Monotonic non-decreasing across the lifetime
    /// of the allocator: freeing an LBA does not lower this value. Used
    /// by btree recovery to bound the rebuild-fallback scan to the region
    /// of the disk that has actually been touched.
    pub fn high_water(&self) -> u64 {
        self.inner.lock().unwrap().max_ever
    }

    /// Raise the high-water mark to at least `hwm`. Never lowers it.
    /// Called by recovery to seed the in-memory HWM from a persisted
    /// value before any new allocations occur.
    pub fn observe_high_water(&self, hwm: u64) {
        let mut g = self.inner.lock().unwrap();
        g.max_ever = g.max_ever.max(hwm);
    }
}

fn word_bit(lba: u64) -> (usize, u32) {
    ((lba / 64) as usize, (lba % 64) as u32)
}

/// Find a run of `n` consecutive zero bits in `[start, end)` and
/// set them all in one pass. Returns the run's starting LBA.
/// The caller must hold the allocator mutex and have verified
/// `used + n <= capacity`.
fn scan_run(inner: &mut Inner, start: u64, end: u64, n: u64) -> Option<u64> {
    if start >= end || end - start < n {
        return None;
    }
    let mut lba = start;
    while lba + n <= end {
        if word_get(&inner.words, lba) {
            lba += 1;
            continue;
        }
        // `lba` is free; greedily extend the run.
        let mut k = 1u64;
        while k < n && !word_get(&inner.words, lba + k) {
            k += 1;
        }
        if k == n {
            for i in 0..n {
                let (w, b) = word_bit(lba + i);
                inner.words[w] |= 1u64 << b;
            }
            inner.used += n;
            inner.next = lba + n;
            inner.max_ever = inner.max_ever.max(lba + n - 1);
            return Some(lba);
        }
        // Hit a used bit at `lba + k`; resume past it.
        lba += k + 1;
    }
    None
}

fn word_get(words: &[u64], lba: u64) -> bool {
    let (w, b) = word_bit(lba);
    words[w] & (1u64 << b) != 0
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
            let inverted = !word;
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
                    inner.max_ever = inner.max_ever.max(candidate);
                    return Some(candidate);
                }
            }
        }
        w += 1;
        lba = (w as u64) * 64;
    }
    None
}
