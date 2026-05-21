// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use super::*;

fn key(i: u32) -> PageKey {
    PageKey::new([0u8; 32], i)
}

#[test]
fn first_touch_rejects_second_admits() {
    let f = AdmissionFilter::new(1024, 2);
    assert!(!f.should_admit(&key(1)));
    assert!(f.should_admit(&key(1)));
}

#[test]
fn distinct_keys_each_need_two_touches() {
    let f = AdmissionFilter::new(1024, 2);
    for i in 0..32u32 {
        assert!(!f.should_admit(&key(i)));
    }
    for i in 0..32u32 {
        assert!(f.should_admit(&key(i)));
    }
}

#[test]
fn record_frequency_bumps_counter() {
    let f = AdmissionFilter::new(64, 4);
    let k = key(7);
    assert_eq!(f.frequency(&k), 0);
    f.record_frequency(&k);
    f.record_frequency(&k);
    assert!(f.frequency(&k) >= 2);
}

#[test]
fn reset_clears_state() {
    let f = AdmissionFilter::new(256, 2);
    assert!(!f.should_admit(&key(1)));
    f.reset();
    // After reset, the doorkeeper is empty again so the next
    // call is the "first touch".
    assert!(!f.should_admit(&key(1)));
}
