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

#[test]
fn aging_clears_doorkeeper_restoring_first_sight() {
    // Aging halves the sketch every `10 * capacity` admits and
    // must clear the doorkeeper in lockstep. Otherwise the bloom
    // fills forever and "admit on second touch" silently decays
    // into "admit on first touch". Use a tiny capacity so the
    // threshold (10 * 4 = 40 admits) is cheap to cross.
    let cap = 4u64;
    let f = AdmissionFilter::new(cap, 2);
    let victim = key(1);
    let threshold = (cap * 10) as usize;

    // First sight: rejected and registered in the doorkeeper.
    assert!(!f.should_admit(&victim));
    // Second sight: admitted (doorkeeper bit is set). This admit
    // is the first one counted toward the aging threshold.
    assert!(f.should_admit(&victim));

    // Only admits (the `true` returns) bump the aging counter.
    // Hammer the same key until the counter sits one admit below
    // the threshold; each call sees the bit set and admits. The
    // doorkeeper has not been cleared yet, so all return true.
    for _ in 0..(threshold - 2) {
        assert!(f.should_admit(&victim));
    }

    // This admit crosses the threshold and triggers the aging
    // step, which clears the doorkeeper. The call itself still
    // observes the bit as set (clearing happens after the probe),
    // so it returns true.
    assert!(f.should_admit(&victim));

    // The doorkeeper is now empty, so the victim is "first sight"
    // again and is rejected: scan resistance is restored.
    assert!(!f.should_admit(&victim));
}
