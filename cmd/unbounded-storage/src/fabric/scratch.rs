// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Shared scratch backing allocator for RPC handlers.

use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};

use crate::memory::Backing;

/// Dedicated scratch backing plus a lock-free stack of free page indices.
pub(crate) struct ScratchBacking {
    backing: Backing,
    head: AtomicU64,
    next: Vec<AtomicU32>,
}

impl ScratchBacking {
    /// Build a scratch allocator over `backing`, making at most
    /// `scratch_pages` slots available for concurrent checkout.
    pub(crate) fn new(backing: Backing, scratch_pages: u32) -> Self {
        let usable = scratch_pages.min(backing.page_count as u32);
        let next = (0..usable)
            .map(|idx| {
                let next = if idx + 1 < usable { idx + 2 } else { 0 };
                AtomicU32::new(next)
            })
            .collect();

        Self {
            backing,
            head: AtomicU64::new(pack(0, if usable == 0 { 0 } else { 1 })),
            next,
        }
    }

    pub(crate) fn page_size(&self) -> usize {
        self.backing.page_size
    }

    /// Pop a free page index and zero the full page before returning it.
    pub(crate) fn take_zeroed(&self) -> Option<u32> {
        let idx = self.take()?;
        let page_size = self.backing.page_size;
        // SAFETY: `idx` came off the free stack, so this request owns
        // the slot exclusively until the matching `give`. The free
        // stack is seeded only with indices below `backing.page_count`.
        unsafe {
            let slot = self.backing.base.add(idx as usize * page_size);
            std::ptr::write_bytes(slot, 0, page_size);
        }
        Some(idx)
    }

    fn take(&self) -> Option<u32> {
        loop {
            let observed = self.head.load(Ordering::Acquire);
            let (tag, idx_plus_one) = unpack(observed);
            if idx_plus_one == 0 {
                return None;
            }

            let idx = idx_plus_one - 1;
            let next = self.next[idx as usize].load(Ordering::Relaxed);
            let desired = pack(tag.wrapping_add(1), next);
            if self
                .head
                .compare_exchange_weak(observed, desired, Ordering::AcqRel, Ordering::Acquire)
                .is_ok()
            {
                return Some(idx);
            }
        }
    }

    pub(crate) fn give(&self, idx: u32) {
        debug_assert!((idx as usize) < self.next.len());
        let node = idx + 1;
        loop {
            let observed = self.head.load(Ordering::Acquire);
            let (tag, head_idx) = unpack(observed);
            self.next[idx as usize].store(head_idx, Ordering::Relaxed);
            let desired = pack(tag.wrapping_add(1), node);
            if self
                .head
                .compare_exchange_weak(observed, desired, Ordering::AcqRel, Ordering::Acquire)
                .is_ok()
            {
                return;
            }
        }
    }
}

fn pack(tag: u32, idx_plus_one: u32) -> u64 {
    ((tag as u64) << 32) | idx_plus_one as u64
}

fn unpack(word: u64) -> (u32, u32) {
    ((word >> 32) as u32, word as u32)
}

#[cfg(test)]
mod tests {
    use super::*;

    const PAGE: usize = 4096;
    const PAGES: usize = 2;

    fn backing() -> (Backing, *mut u8) {
        let buf = vec![0xA5u8; PAGE * PAGES].into_boxed_slice();
        let base = Box::leak(buf).as_mut_ptr();
        let backing = Backing {
            base,
            page_size: PAGE,
            page_count: PAGES,
            keepalive: std::sync::Arc::new(()),
        };
        (backing, base)
    }

    #[test]
    fn checkout_exhausts_and_recycles_pages() {
        let (backing, _base) = backing();
        let scratch = ScratchBacking::new(backing, PAGES as u32);

        let a = scratch.take_zeroed().expect("first page");
        let b = scratch.take_zeroed().expect("second page");
        assert!(scratch.take_zeroed().is_none());

        scratch.give(a);
        assert_eq!(scratch.take_zeroed(), Some(a));
        scratch.give(b);
    }

    #[test]
    fn checkout_zeroes_recycled_page() {
        let (backing, base) = backing();
        let scratch = ScratchBacking::new(backing, 1);
        let idx = scratch.take_zeroed().expect("page");

        // SAFETY: idx came from the one-page scratch backing.
        unsafe {
            std::ptr::write_bytes(base.add(idx as usize * PAGE), 0x5A, PAGE);
        }

        scratch.give(idx);
        let idx = scratch.take_zeroed().expect("recycled page");

        // SAFETY: idx came from the one-page scratch backing.
        let zeroed = unsafe { std::slice::from_raw_parts(base.add(idx as usize * PAGE), PAGE) };
        assert!(zeroed.iter().all(|&b| b == 0));
    }
}
