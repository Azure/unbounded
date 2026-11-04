//! Counting global allocator for allocation probes.
//!
//! The type is only installed by the dedicated `alloc_probe` integration test
//! binary, so other test binaries keep the system allocator. Threads that call
//! [`exclude_current_thread`] (benchmark clients and drivers) are not counted,
//! which isolates allocations made by the dataplane worker threads.
use std::alloc::GlobalAlloc;
use std::alloc::Layout;
use std::alloc::System;
use std::cell::Cell;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;

pub struct Counting;

static ALLOCATIONS: AtomicU64 = AtomicU64::new(0);
static BYTES: AtomicU64 = AtomicU64::new(0);

thread_local! {
    static EXCLUDED: Cell<bool> = const { Cell::new(false) };
}

fn record(size: usize) {
    // try_with avoids touching thread-local storage during thread teardown.
    if EXCLUDED.try_with(Cell::get).unwrap_or(true) {
        return;
    }
    ALLOCATIONS.fetch_add(1, Ordering::Relaxed);
    BYTES.fetch_add(size as u64, Ordering::Relaxed);
}

// SAFETY: every method forwards to the system allocator unchanged.
unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        record(layout.size());
        // SAFETY: forwarded caller contract.
        unsafe { System.alloc(layout) }
    }
    unsafe fn alloc_zeroed(&self, layout: Layout) -> *mut u8 {
        record(layout.size());
        // SAFETY: forwarded caller contract.
        unsafe { System.alloc_zeroed(layout) }
    }
    unsafe fn realloc(&self, ptr: *mut u8, layout: Layout, new_size: usize) -> *mut u8 {
        record(new_size);
        // SAFETY: forwarded caller contract.
        unsafe { System.realloc(ptr, layout, new_size) }
    }
    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        // SAFETY: forwarded caller contract.
        unsafe { System.dealloc(ptr, layout) }
    }
}

/// Stop counting allocations made by the calling thread.
pub fn exclude_current_thread() {
    EXCLUDED.with(|excluded| excluded.set(true));
}

/// Allocation calls (alloc, alloc_zeroed and realloc) and requested bytes so far.
pub fn snapshot() -> (u64, u64) {
    (
        ALLOCATIONS.load(Ordering::Relaxed),
        BYTES.load(Ordering::Relaxed),
    )
}
