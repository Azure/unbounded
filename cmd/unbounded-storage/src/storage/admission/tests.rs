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

// Regression: the doorkeeper bloom used to never be cleared, so after
// roughly `capacity_bits / NUM_HASHES` distinct touches every probe
// returned `all_set` and `should_admit` degenerated to "admit
// everything" on the very first touch of any new key. Drive the
// filter well past the bloom's saturation point with synthetic
// distinct keys and confirm a brand-new key still gets rejected on
// its first touch.
#[test]
fn doorkeeper_resets_under_saturation() {
    let capacity: u32 = 256;
    let f = AdmissionFilter::new(capacity as u64, 2);
    // 50x the configured capacity is several full saturation cycles;
    // before the fix this would leave every doorkeeper bit set.
    for i in 0..(capacity * 50) {
        let _ = f.should_admit(&key(i));
    }
    // A key that has never been touched must still be a first-touch
    // miss. Use a namespace well above any key probed above.
    let fresh = PageKey::new([1u8; 32], u32::MAX);
    assert!(
        !f.should_admit(&fresh),
        "fresh key was admitted on first touch: doorkeeper saturated"
    );
}

// Steady-state workload that stays *within* a single doorkeeper
// window must still reject first-touches. This guards against the
// opposite failure mode: clearing the doorkeeper so aggressively
// that the second-touch policy stops working for normal workloads.
#[test]
fn doorkeeper_still_rejects_first_touch_in_steady_state() {
    let capacity: u32 = 1024;
    let f = AdmissionFilter::new(capacity as u64, 2);
    // Half a window of distinct first-touches: nothing should be
    // admitted because each key has only been seen once.
    let window = capacity / 2;
    for i in 0..window {
        assert!(
            !f.should_admit(&key(i)),
            "key {i} admitted on first touch in steady state"
        );
    }
}

// Classic two-touch admission within a single doorkeeper window.
// Touch each key once (all rejected), then again (all admitted).
// Total probes are kept well under `capacity_pages` so no clear
// fires between the two passes.
#[test]
fn second_touch_within_window_admits() {
    let capacity: u32 = 1024;
    let f = AdmissionFilter::new(capacity as u64, 2);
    let n = capacity / 8; // 2 * n probes total, far below threshold
    for i in 0..n {
        assert!(!f.should_admit(&key(i)));
    }
    for i in 0..n {
        assert!(
            f.should_admit(&key(i)),
            "key {i} not admitted on second touch within window"
        );
    }
}
