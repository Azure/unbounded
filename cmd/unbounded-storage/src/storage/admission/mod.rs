// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! TinyLFU-style admission filter.
//!
//! Implements the design's "admit on second touch" policy via a
//! doorkeeper bloom filter. The first time a [`PageKey`] is
//! offered, [`should_admit`](AdmissionFilter::should_admit)
//! returns `false` and the key is recorded; subsequent calls
//! within the same epoch return `true`. This filters one-hit
//! wonders from polluting the resident set.
//!
//! The full TinyLFU adds a count-min sketch for frequency
//! tracking. We keep a sketch as a feature-flag stub
//! ([`record_frequency`](AdmissionFilter::record_frequency) /
//! [`frequency`](AdmissionFilter::frequency)) so the engine can be
//! extended later; the admission decision today only consults
//! the doorkeeper. Views that only need the doorkeeper construct
//! via [`AdmissionFilter::new_doorkeeper_only`] to skip the
//! sketch allocation and per-admit bumps entirely.
//!
//! All state is in memory and is intentionally rebuilt empty
//! on restart - bloom mistakes are bounded and the cache is
//! warm-up tolerant.
//!
//! ## Concurrency
//!
//! The hot path ([`should_admit`](AdmissionFilter::should_admit),
//! [`record_frequency`](AdmissionFilter::record_frequency),
//! [`frequency`](AdmissionFilter::frequency)) is lock-free. The
//! doorkeeper, sketch, and aging counter are all atomics accessed
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
use std::sync::atomic::{AtomicU8, AtomicU64, Ordering};

use crate::storage::types::{GOLDEN_RATIO_64, PageKey};

mod stripe;
pub use stripe::{AdmitDecision, StripeAdmission};

pub struct AdmissionFilter {
    // Doorkeeper bloom: bits packed into AtomicU64 words. Set
    // via `fetch_or(mask, Relaxed)`; the returned prior value
    // tells us whether the bit was already set.
    doorkeeper: Box<[AtomicU64]>,
    capacity_bits: u64,
    // Count-min sketch, one row per hash. `None` when the filter
    // is in doorkeeper-only mode (see `new_doorkeeper_only`).
    sketch: Option<[Box<[AtomicU8]>; SKETCH_ROWS]>,
    sketch_width: u32,
    // Aging is driven by a counter of admits since the last age
    // step. When it crosses `reset_threshold`, exactly one writer
    // takes `aging` and halves the sketch counters.
    inserts_since_reset: AtomicU64,
    reset_threshold: u64,
    aging: Mutex<()>,
}

const NUM_HASHES: u32 = 3;
const SKETCH_ROWS: usize = 4;

// Domain tag for the doorkeeper bloom filter probes. Not a
// secret; see PageKey::mix. Sketch rows use domains 1..=SKETCH_ROWS via
// sketch_index, so 0 is reserved for the doorkeeper.
const DOORKEEPER_DOMAIN: u32 = 0;

// Bloom sizing multiplier. With k = 3 hashes the FPR is
// (1 - e^(-3n/m))^3; honoring "a few million entries at ~1% FPR"
// requires m / capacity >= ~12.3. Round to 12.
const BITS_PER_ENTRY: u64 = 12;

impl AdmissionFilter {
    /// Full filter with both doorkeeper and count-min sketch.
    pub fn new(capacity_pages: u64, sketch_multiplier: u32) -> Self {
        let (doorkeeper, capacity_bits) = make_doorkeeper(capacity_pages);
        let sketch_width = (capacity_pages.max(1) as u32).saturating_mul(sketch_multiplier.max(1));
        let sketch = make_sketch(sketch_width);
        Self {
            doorkeeper,
            capacity_bits,
            sketch: Some(sketch),
            sketch_width,
            inserts_since_reset: AtomicU64::new(0),
            // Halve the sketch (aging) every ~10 * capacity admits.
            reset_threshold: capacity_pages.max(1).saturating_mul(10),
            aging: Mutex::new(()),
        }
    }

    /// Doorkeeper-only filter: no count-min sketch is allocated,
    /// `record_frequency` is a no-op, and `frequency` returns 0.
    /// Used by views (see [`StripeAdmission`]) that only need the
    /// second-sight admission decision.
    pub fn new_doorkeeper_only(capacity_pages: u64) -> Self {
        let (doorkeeper, capacity_bits) = make_doorkeeper(capacity_pages);
        Self {
            doorkeeper,
            capacity_bits,
            sketch: None,
            sketch_width: 0,
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
            if let Some(sketch) = &self.sketch {
                sketch_bump(sketch, self.sketch_width, key);
                let prev = self.inserts_since_reset.fetch_add(1, Ordering::Relaxed);
                if prev + 1 >= self.reset_threshold {
                    self.maybe_age(sketch);
                }
            }
        }
        admit
    }

    /// Increment the sketch without consulting the doorkeeper.
    /// Exposed so the engine can record hits as well as admits.
    /// No-op in doorkeeper-only mode.
    pub fn record_frequency(&self, key: &PageKey) {
        if let Some(sketch) = &self.sketch {
            sketch_bump(sketch, self.sketch_width, key);
        }
    }

    /// Reset all state. Tests and explicit warmup boundaries
    /// call this. Restart is handled at engine construction by
    /// simply not persisting any of this state.
    pub fn reset(&self) {
        // Hold the aging mutex so a concurrent aging step cannot
        // race the wipe.
        let _g = self.aging.lock().unwrap();
        clear_doorkeeper(&self.doorkeeper);
        if let Some(sketch) = &self.sketch {
            for row in sketch.iter() {
                for c in row.iter() {
                    c.store(0, Ordering::Relaxed);
                }
            }
        }
        self.inserts_since_reset.store(0, Ordering::Relaxed);
    }

    /// Approximate frequency estimate from the count-min
    /// sketch. Used by callers that want to make their own
    /// decision (e.g. compare against a victim's estimate).
    /// Returns 0 in doorkeeper-only mode.
    pub fn frequency(&self, key: &PageKey) -> u8 {
        let Some(sketch) = &self.sketch else {
            return 0;
        };
        let mut min = u8::MAX;
        for (i, row) in sketch.iter().enumerate() {
            let idx = sketch_index(key, i as u32, self.sketch_width);
            min = min.min(row[idx].load(Ordering::Relaxed));
        }
        min
    }

    fn maybe_age(&self, sketch: &[Box<[AtomicU8]>; SKETCH_ROWS]) {
        // Serialize aging on its own mutex. Multiple writers that
        // also observed threshold-crossed will block briefly, then
        // double-check the counter and exit if another writer
        // already aged.
        let _g = self.aging.lock().unwrap();
        if self.inserts_since_reset.load(Ordering::Relaxed) < self.reset_threshold {
            return;
        }
        age(sketch);
        // Clear the doorkeeper in lockstep with the sketch reset.
        // Without this the bloom fills monotonically over the
        // process lifetime, its FPR climbs toward 1.0, and
        // "admit on second touch" degrades to "admit on first
        // touch", defeating scan resistance (canonical W-TinyLFU
        // resets the doorkeeper alongside each aging period).
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

fn make_sketch(width: u32) -> [Box<[AtomicU8]>; SKETCH_ROWS] {
    std::array::from_fn(|_| {
        let mut row = Vec::with_capacity(width as usize);
        row.resize_with(width as usize, || AtomicU8::new(0));
        row.into_boxed_slice()
    })
}

// Zero every doorkeeper word. Called under the `aging` mutex
// from both `reset` and the aging step so a wipe cannot race a
// concurrent age. Relaxed matches the rest of this probabilistic
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

fn sketch_bump(rows: &[Box<[AtomicU8]>; SKETCH_ROWS], width: u32, key: &PageKey) {
    for i in 0..SKETCH_ROWS {
        let idx = sketch_index(key, i as u32, width);
        let _ = rows[i][idx].fetch_update(Ordering::Relaxed, Ordering::Relaxed, |c| {
            if c == u8::MAX { None } else { Some(c + 1) }
        });
    }
}

fn sketch_index(key: &PageKey, row: u32, width: u32) -> usize {
    let h = key.mix(row.wrapping_add(1));
    (h % (width as u64)) as usize
}

fn age(rows: &[Box<[AtomicU8]>; SKETCH_ROWS]) {
    for row in rows.iter() {
        for c in row.iter() {
            // Halve. Relaxed is fine: aging is approximate and
            // serialized via the aging mutex against other agers.
            let v = c.load(Ordering::Relaxed);
            c.store(v >> 1, Ordering::Relaxed);
        }
    }
}

#[cfg(test)]
mod tests;
