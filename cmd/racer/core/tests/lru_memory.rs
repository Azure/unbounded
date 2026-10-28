//! Measures the heap cost of payload-free memory cache indexes per entry.

use racer_core::cache::{OrderedLru, TickLru};
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

/// Bytes per entry for `u64` keys and `u32` values, after `count` inserts.
fn per_entry(count: u64, build: impl FnOnce(u64) -> Box<dyn std::any::Any>) -> usize {
    let before = LIVE.load(Ordering::Relaxed);
    let value = build(count);
    let used = LIVE.load(Ordering::Relaxed) - before;
    drop(value);
    used / usize::try_from(count).unwrap()
}

#[test]
fn phantom_lru_entries_stay_small() {
    let tick = per_entry(100_000, |count| {
        let mut lru = TickLru::<u64, u32>::default();
        for id in 0..count {
            lru.insert(id, 1).unwrap();
        }
        Box::new(lru)
    });
    let ordered = per_entry(100_000, |count| {
        let mut lru = OrderedLru::<u64, u32>::default();
        for id in 0..count {
            lru.insert(id, 1);
        }
        Box::new(lru)
    });
    println!("phantom memory LRU: tick {tick} B/entry, ordered {ordered} B/entry");
    assert!(tick <= 96, "{tick}");
    assert!(ordered <= 128, "{ordered}");
}
