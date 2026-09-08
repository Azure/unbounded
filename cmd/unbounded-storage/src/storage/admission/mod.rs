// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Second-touch admission filter.
//!
//! Implements the design's "admit on second touch" policy via a
//! doorkeeper bloom filter. The first time a [`PageKey`] is
//! offered, [`should_admit`](AdmissionFilter::should_admit)
//! returns `false` and the key is recorded; subsequent calls
//! within the same epoch return `true`. This filters one-hit
//! wonders from polluting the resident set.
//!
//! All state is in memory and is intentionally rebuilt empty
//! on restart - bloom mistakes are bounded and the cache is
//! warm-up tolerant.
//!
//! ## Concurrency
//!
//! The hot path ([`should_admit`](AdmissionFilter::should_admit))
//! is lock-free. The doorkeeper and aging counter are atomics accessed
//! with [`Ordering::Relaxed`]: this is a probabilistic structure
//! and we do not build a happens-before relationship through it.
//! The only mutex is a small one guarding the rare aging step
//! (and `reset`), so aging contention is bounded to "epoch
//! boundary" rather than "every admit".
//!
//! Under concurrent admits for the same key, multiple racers can
//! each see the doorkeeper bits unset and each return `false`
//! once. This is acceptable: the filter is probabilistic and the
//! eventual-admit semantics are preserved.

use std::sync::Mutex;
use std::sync::atomic::{AtomicU64, Ordering};

use crate::storage::types::{GOLDEN_RATIO_64, PageKey};

mod stripe;
pub use stripe::{AdmitDecision, StripeAdmission};

pub struct AdmissionFilter {
    // Doorkeeper bloom: bits packed into AtomicU64 words. Set
    // via `fetch_or(mask, Relaxed)`; the returned prior value
    // tells us whether the bit was already set.
    doorkeeper: Box<[AtomicU64]>,
    capacity_bits: u64,
    // Resetting is driven by a counter of admits since the last
    // reset. When it crosses `reset_threshold`, exactly one writer
    // takes `aging` and clears the doorkeeper.
    inserts_since_reset: AtomicU64,
    reset_threshold: u64,
    aging: Mutex<()>,
}

const NUM_HASHES: u32 = 3;

// Domain tag for the doorkeeper bloom filter probes. Not a
// secret; see PageKey::mix.
const DOORKEEPER_DOMAIN: u32 = 0;

// Bloom sizing multiplier. With k = 3 hashes the FPR is
// (1 - e^(-3n/m))^3; honoring "a few million entries at ~1% FPR"
// requires m / capacity >= ~12.3. Round to 12.
const BITS_PER_ENTRY: u64 = 12;

impl AdmissionFilter {
    pub fn new(capacity_pages: u64) -> Self {
        let (doorkeeper, capacity_bits) = make_doorkeeper(capacity_pages);
        Self {
            doorkeeper,
            capacity_bits,
            inserts_since_reset: AtomicU64::new(0),
            // Reset the doorkeeper every ~10 * capacity admits.
            reset_threshold: capacity_pages.max(1).saturating_mul(10),
            aging: Mutex::new(()),
        }
    }

    /// Non-aging filter used by views whose lifetime defines the epoch.
    pub fn new_doorkeeper_only(capacity_pages: u64) -> Self {
        let (doorkeeper, capacity_bits) = make_doorkeeper(capacity_pages);
        Self {
            doorkeeper,
            capacity_bits,
            inserts_since_reset: AtomicU64::new(0),
            reset_threshold: u64::MAX,
            aging: Mutex::new(()),
        }
    }

    /// Return true if `key` should be admitted to the resident
    /// set. The first call for a given key returns false and
    /// registers the key in the doorkeeper; the second call (or
    /// later, within the same epoch) returns true.
    ///
    /// Under concurrency, two racers can each observe "first
    /// touch" for the same key and both return false; this is
    /// acceptable (see the module-level concurrency note).
    pub fn should_admit(&self, key: &PageKey) -> bool {
        let admit = doorkeeper_probe_and_set(&self.doorkeeper, self.capacity_bits, key);
        if admit {
            let prev = self.inserts_since_reset.fetch_add(1, Ordering::Relaxed);
            if prev + 1 >= self.reset_threshold {
                self.maybe_reset();
            }
        }
        admit
    }

    /// Reset all state. Tests and explicit warmup boundaries
    /// call this. Restart is handled at engine construction by
    /// simply not persisting any of this state.
    pub fn reset(&self) {
        // Hold the aging mutex so a concurrent reset cannot
        // race the wipe.
        let _g = self.aging.lock().unwrap();
        clear_doorkeeper(&self.doorkeeper);
        self.inserts_since_reset.store(0, Ordering::Relaxed);
    }

    fn maybe_reset(&self) {
        // Serialize resetting on its own mutex. Multiple writers that
        // also observed threshold-crossed will block briefly, then
        // double-check the counter and exit if another writer
        // already reset it.
        let _g = self.aging.lock().unwrap();
        if self.inserts_since_reset.load(Ordering::Relaxed) < self.reset_threshold {
            return;
        }
        // Without resetting, the bloom fills monotonically over the
        // process lifetime, its FPR climbs toward 1.0, and
        // "admit on second touch" degrades to "admit on first
        // touch", defeating scan resistance.
        clear_doorkeeper(&self.doorkeeper);
        self.inserts_since_reset.store(0, Ordering::Relaxed);
    }
}

fn make_doorkeeper(capacity_pages: u64) -> (Box<[AtomicU64]>, u64) {
    let capacity_bits = capacity_pages.max(1).saturating_mul(BITS_PER_ENTRY).max(64);
    let words = capacity_bits.div_ceil(64) as usize;
    let mut v = Vec::with_capacity(words);
    v.resize_with(words, || AtomicU64::new(0));
    (v.into_boxed_slice(), capacity_bits)
}

// Zero every doorkeeper word. Called under the `aging` mutex so
// explicit and threshold-driven resets cannot race. Relaxed matches
// the rest of this probabilistic
// structure (see the module-level concurrency note).
fn clear_doorkeeper(words: &[AtomicU64]) {
    for w in words.iter() {
        w.store(0, Ordering::Relaxed);
    }
}

fn doorkeeper_probe_and_set(words: &[AtomicU64], bits: u64, key: &PageKey) -> bool {
    let h = key.mix(DOORKEEPER_DOMAIN);
    let mut all_set = true;
    for i in 0..NUM_HASHES {
        let bit = h.wrapping_add((i as u64).wrapping_mul(GOLDEN_RATIO_64)) % bits;
        let word = (bit / 64) as usize;
        let mask = 1u64 << (bit % 64);
        let prev = words[word].fetch_or(mask, Ordering::Relaxed);
        if prev & mask == 0 {
            all_set = false;
        }
    }
    all_set
}

#[cfg(test)]
mod tests;
