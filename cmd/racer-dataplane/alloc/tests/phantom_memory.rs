//! Measures the heap cost of payload-free segment entries per page.

use page_alloc::{PhantomEntries, SegmentId};
use std::{
    alloc::{GlobalAlloc, Layout, System},
    sync::atomic::{AtomicUsize, Ordering},
};

/// Counts live heap bytes for this test binary.
struct Counting;

static LIVE: AtomicUsize = AtomicUsize::new(0);

// SAFETY: forwards to the system allocator and only adds counting.
unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        LIVE.fetch_add(layout.size(), Ordering::Relaxed);
        // SAFETY: same layout contract as the caller's.
        unsafe { System.alloc(layout) }
    }

    unsafe fn dealloc(&self, ptr: *mut u8, layout: Layout) {
        LIVE.fetch_sub(layout.size(), Ordering::Relaxed);
        // SAFETY: same pointer and layout the caller allocated with.
        unsafe { System.dealloc(ptr, layout) }
    }
}

#[global_allocator]
static GLOBAL: Counting = Counting;

/// Upper bound on bytes per page with `u64` keys, used by the simulator's
/// memory accounting.
const BYTES_PER_PAGE: usize = 160;

#[test]
fn phantom_entries_stay_small_per_page() {
    let pages = 100_000u64;
    let before = LIVE.load(Ordering::Relaxed);
    let entries = PhantomEntries::<u64>::default();
    for page in 0..pages {
        entries.publish(page, 4096, SegmentId(page / 64)).unwrap();
    }
    let used = LIVE.load(Ordering::Relaxed) - before;
    let per_page = used / usize::try_from(pages).unwrap();
    println!("phantom segment entries: {per_page} bytes per page");
    assert!(per_page <= BYTES_PER_PAGE, "{per_page}");
    drop(entries);
}
