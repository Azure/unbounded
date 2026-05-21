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
