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
