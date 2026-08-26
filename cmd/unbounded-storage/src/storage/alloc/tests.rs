// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![cfg(test)]

use super::Allocator;
use crate::storage::types::{Error, Lba};

#[test]
fn alloc_returns_distinct_lbas() {
    let a = Allocator::new(128);
    let mut seen = std::collections::HashSet::new();
    for _ in 0..128 {
        let lba = a.alloc().unwrap();
        assert!(seen.insert(lba.0));
    }
    assert!(matches!(a.alloc(), Err(Error::OutOfSpace)));
}

#[test]
fn free_returns_lba_to_pool() {
    let a = Allocator::new(4);
    let l0 = a.alloc().unwrap();
    let l1 = a.alloc().unwrap();
    a.alloc().unwrap();
    a.alloc().unwrap();
    assert!(matches!(a.alloc(), Err(Error::OutOfSpace)));
    a.free(l0).unwrap();
    a.free(l1).unwrap();
    assert_eq!(a.free_pages(), 2);
    let r0 = a.alloc().unwrap();
    let r1 = a.alloc().unwrap();
    assert_ne!(r0, r1);
    assert!(matches!(a.alloc(), Err(Error::OutOfSpace)));
}

#[test]
fn mark_in_use_is_idempotent() {
    let a = Allocator::new(16);
    a.mark_in_use(Lba(3)).unwrap();
    a.mark_in_use(Lba(3)).unwrap();
    assert_eq!(a.used_pages(), 1);
    assert!(a.is_in_use(Lba(3)).unwrap());
    assert!(!a.is_in_use(Lba(4)).unwrap());
}

#[test]
fn alloc_skips_marked_in_use() {
    let a = Allocator::new(8);
    for lba in [Lba(0), Lba(1), Lba(3)] {
        a.mark_in_use(lba).unwrap();
    }
    let n = a.alloc().unwrap();
    assert_eq!(n, Lba(2));
}

#[test]
fn free_rejects_out_of_range() {
    let a = Allocator::new(4);
    assert!(matches!(a.free(Lba(10)), Err(Error::OutOfRange)));
    assert!(matches!(a.mark_in_use(Lba(10)), Err(Error::OutOfRange)));
    assert!(matches!(a.is_in_use(Lba(10)), Err(Error::OutOfRange)));
}

#[test]
fn capacity_not_multiple_of_64_works() {
    let a = Allocator::new(70);
    for _ in 0..70 {
        a.alloc().unwrap();
    }
    assert!(matches!(a.alloc(), Err(Error::OutOfSpace)));
}

#[test]
fn used_and_free_accounting() {
    let a = Allocator::new(10);
    assert_eq!(a.used_pages(), 0);
    assert_eq!(a.free_pages(), 10);
    let l = a.alloc().unwrap();
    assert_eq!(a.used_pages(), 1);
    a.free(l).unwrap();
    assert_eq!(a.used_pages(), 0);
}

#[test]
fn high_water_starts_zero() {
    let a = Allocator::new(128);
    assert_eq!(a.high_water(), 0);
}

#[test]
fn high_water_tracks_alloc() {
    let a = Allocator::new(128);
    let mut max_seen = 0u64;
    for _ in 0..10 {
        let lba = a.alloc().unwrap();
        max_seen = max_seen.max(lba.0);
    }
    assert_eq!(a.high_water(), max_seen);
}

#[test]
fn high_water_tracks_mark_in_use() {
    let a = Allocator::new(128);
    a.mark_in_use(Lba(42)).unwrap();
    assert!(a.high_water() >= 42);
    a.mark_in_use(Lba(7)).unwrap();
    assert!(a.high_water() >= 42);
}

#[test]
fn high_water_monotonic_on_free() {
    let a = Allocator::new(128);
    let mut lbas = Vec::new();
    for _ in 0..5 {
        lbas.push(a.alloc().unwrap());
    }
    let hwm_before = a.high_water();
    for l in lbas {
        a.free(l).unwrap();
    }
    assert_eq!(a.high_water(), hwm_before);
}

#[test]
fn observe_high_water_raises() {
    let a = Allocator::new(1024);
    a.observe_high_water(100);
    assert_eq!(a.high_water(), 100);
}

#[test]
fn observe_high_water_never_lowers() {
    let a = Allocator::new(1024);
    a.observe_high_water(100);
    a.observe_high_water(50);
    assert_eq!(a.high_water(), 100);
}

#[test]
fn mark_range_in_use_marks_every_lba_in_run() {
    let a = Allocator::new(64);
    a.mark_range_in_use(Lba(10), 5).unwrap();
    for lba in 10..15 {
        assert!(a.is_in_use(Lba(lba)).unwrap(), "lba {lba} not marked");
    }
    assert!(!a.is_in_use(Lba(9)).unwrap());
    assert!(!a.is_in_use(Lba(15)).unwrap());
    assert_eq!(a.used_pages(), 5);
    assert_eq!(a.high_water(), 14);
}

#[test]
fn mark_range_in_use_is_idempotent() {
    let a = Allocator::new(64);
    a.mark_range_in_use(Lba(10), 3).unwrap();
    a.mark_range_in_use(Lba(10), 3).unwrap();
    assert_eq!(a.used_pages(), 3);
}

#[test]
fn mark_range_in_use_blocks_subsequent_alloc() {
    let a = Allocator::new(16);
    // Reserve a run; the very next alloc must not collide with
    // any LBA inside it. This is the scenario the btree open
    // path depends on: a multi-LBA leaf-entry run must be
    // fully recorded so `alloc` / `alloc_contig` skip it.
    a.mark_range_in_use(Lba(0), 8).unwrap();
    for _ in 0..(16 - 8) {
        let l = a.alloc().unwrap();
        assert!(l.0 >= 8, "alloc handed out lba {} inside marked run", l.0);
    }
    assert!(matches!(a.alloc(), Err(Error::OutOfSpace)));
}

#[test]
fn mark_range_in_use_rejects_overflow() {
    let a = Allocator::new(16);
    assert!(matches!(
        a.mark_range_in_use(Lba(15), 2),
        Err(Error::OutOfRange)
    ));
    assert!(matches!(
        a.mark_range_in_use(Lba(u64::MAX), 1),
        Err(Error::OutOfRange)
    ));
}

#[test]
fn mark_range_in_use_zero_is_noop() {
    let a = Allocator::new(16);
    a.mark_range_in_use(Lba(5), 0).unwrap();
    assert_eq!(a.used_pages(), 0);
    assert_eq!(a.high_water(), 0);
}

#[test]
fn alloc_contig_happy_path_on_fresh_allocator() {
    let a = Allocator::new(64);
    let start = a.alloc_contig(8).unwrap();
    assert_eq!(start, Lba(0));
    for lba in 0..8 {
        assert!(a.is_in_use(Lba(lba)).unwrap());
    }
    assert_eq!(a.used_pages(), 8);
    assert_eq!(a.high_water(), 7);
    // `next` advanced to start + n: the next single alloc must
    // come from LBA 8, not from inside the just-allocated run.
    let after = a.alloc().unwrap();
    assert_eq!(after, Lba(8));
}

#[test]
fn alloc_contig_zero_rejects() {
    let a = Allocator::new(16);
    assert!(matches!(a.alloc_contig(0), Err(Error::OutOfRange)));
    assert_eq!(a.used_pages(), 0);
}

#[test]
fn alloc_contig_one_matches_alloc() {
    let a = Allocator::new(16);
    let l = a.alloc_contig(1).unwrap();
    assert_eq!(l, Lba(0));
    assert_eq!(a.used_pages(), 1);
    assert_eq!(a.high_water(), 0);
    let next = a.alloc().unwrap();
    assert_eq!(next, Lba(1));
}

#[test]
fn alloc_contig_out_of_space_when_insufficient_free() {
    let a = Allocator::new(8);
    for _ in 0..6 {
        a.alloc().unwrap();
    }
    // used = 6, n = 3, used + n = 9 > capacity = 8.
    assert!(matches!(a.alloc_contig(3), Err(Error::OutOfSpace)));
}

#[test]
fn alloc_contig_finds_first_fit_through_fragmentation() {
    let a = Allocator::new(64);
    // Scatter in-use bits so the first 4-wide gap from the
    // hint is at LBA 5. `scan_run` resumes past the bit that
    // broke the run, so the prior isolated free slots at
    // LBAs 1 and 3 are skipped without backtracking.
    a.mark_in_use(Lba(0)).unwrap();
    a.mark_in_use(Lba(2)).unwrap();
    a.mark_in_use(Lba(4)).unwrap();
    let start = a.alloc_contig(4).unwrap();
    assert_eq!(start, Lba(5));
    for lba in 5..9 {
        assert!(a.is_in_use(Lba(lba)).unwrap());
    }
    assert!(!a.is_in_use(Lba(1)).unwrap());
    assert!(!a.is_in_use(Lba(3)).unwrap());
}

#[test]
fn alloc_contig_wraps_when_hint_region_exhausted() {
    let a = Allocator::new(16);
    // Fill the disk, then carve out one contiguous gap at the
    // low end and advance `next` past it via a one-off free +
    // single alloc so the second `scan_run(0, start, n)` pass
    // is the one that finds the run.
    let mut lbas = Vec::new();
    for _ in 0..16 {
        lbas.push(a.alloc().unwrap());
    }
    // Free the low run [0, 4).
    for l in &lbas[0..4] {
        a.free(*l).unwrap();
    }
    // Free a single high LBA so the next alloc lands there and
    // advances `next` past 4, leaving the only contig-4 gap
    // strictly behind the hint.
    a.free(lbas[10]).unwrap();
    let one = a.alloc().unwrap();
    assert_eq!(one, Lba(10));
    // Now next > 4 and [0, 4) is the only 4-wide free run.
    let start = a.alloc_contig(4).unwrap();
    assert_eq!(start, Lba(0));
    for lba in 0..4 {
        assert!(a.is_in_use(Lba(lba)).unwrap());
    }
}

#[test]
fn alloc_contig_no_fit_returns_out_of_space() {
    let a = Allocator::new(8);
    // Single in-use bit at LBA 3 splits the disk into [0, 3)
    // and [4, 8); neither is 6 wide even though used + n = 7
    // is within capacity.
    a.mark_in_use(Lba(3)).unwrap();
    assert!(matches!(a.alloc_contig(6), Err(Error::OutOfSpace)));
    // No partial state was committed.
    assert_eq!(a.used_pages(), 1);
}

#[test]
fn free_range_happy_path() {
    let a = Allocator::new(64);
    let start = a.alloc_contig(8).unwrap();
    assert_eq!(a.used_pages(), 8);
    a.free_range(start, 8).unwrap();
    for lba in 0..8 {
        assert!(!a.is_in_use(Lba(lba)).unwrap());
    }
    assert_eq!(a.used_pages(), 0);
    // free_range resets `next` to the start of the freed run,
    // so the next alloc_contig of the same size returns it.
    let again = a.alloc_contig(8).unwrap();
    assert_eq!(again, start);
}

#[test]
fn free_range_zero_is_noop() {
    let a = Allocator::new(16);
    let l = a.alloc().unwrap();
    let used_before = a.used_pages();
    a.free_range(Lba(5), 0).unwrap();
    assert_eq!(a.used_pages(), used_before);
    // The previously-allocated bit is still set.
    assert!(a.is_in_use(l).unwrap());
}

#[test]
fn free_range_overflow_rejects() {
    let a = Allocator::new(16);
    assert!(matches!(
        a.free_range(Lba(u64::MAX), 2),
        Err(Error::OutOfRange)
    ));
}

#[test]
fn free_range_past_capacity_rejects() {
    let a = Allocator::new(16);
    assert!(matches!(a.free_range(Lba(15), 2), Err(Error::OutOfRange)));
    // No state mutation on rejection.
    assert_eq!(a.used_pages(), 0);
}

#[test]
fn free_range_skips_already_free_bits_without_underflow() {
    let a = Allocator::new(32);
    // Allocate only a couple of LBAs inside the span we will free.
    a.mark_in_use(Lba(5)).unwrap();
    a.mark_in_use(Lba(7)).unwrap();
    assert_eq!(a.used_pages(), 2);
    // Free a wider span covering both the in-use bits and a
    // mix of already-free LBAs. Only the in-use bits should
    // decrement `used`; the free bits are skipped, no underflow.
    a.free_range(Lba(4), 6).unwrap();
    for lba in 4..10 {
        assert!(!a.is_in_use(Lba(lba)).unwrap());
    }
    assert_eq!(a.used_pages(), 0);
}

#[test]
fn free_range_followed_by_alloc_contig_returns_same_start() {
    let a = Allocator::new(64);
    // Burn a prefix so the freed run isn't at LBA 0; this
    // proves alloc_contig honors the hint reset, not just the
    // first-fit-from-zero behavior.
    for _ in 0..4 {
        a.alloc().unwrap();
    }
    let start = a.alloc_contig(5).unwrap();
    assert_eq!(start, Lba(4));
    a.free_range(start, 5).unwrap();
    let again = a.alloc_contig(5).unwrap();
    assert_eq!(again, start);
}

#[test]
fn observe_high_water_below_current_allocations() {
    let a = Allocator::new(1024);
    let mut max_seen = 0u64;
    for _ in 0..20 {
        let lba = a.alloc().unwrap();
        max_seen = max_seen.max(lba.0);
    }
    let hwm_before = a.high_water();
    assert_eq!(hwm_before, max_seen);
    a.observe_high_water(max_seen / 2);
    assert_eq!(a.high_water(), hwm_before);
}
