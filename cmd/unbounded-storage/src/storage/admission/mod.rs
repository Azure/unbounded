// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! TinyLFU-style admission filter.
//!
//! Implements the design's "admit on second touch" policy via a
//! doorkeeper bloom filter. The first time a [`PageKey`] is
//! offered, [`should_admit`] returns `false` and the key is
//! recorded; subsequent calls within the same epoch return
//! `true`. This filters one-hit-wonders from polluting the
//! resident set.
//!
//! The full TinyLFU adds a count-min sketch for frequency
//! tracking. We keep a sketch as a feature-flag stub
//! ([`record_frequency`] / [`frequency`]) so the engine can be
//! extended later; the admission decision today only consults
//! the doorkeeper.
//!
//! All state is in memory and is intentionally rebuilt empty
//! on restart - bloom mistakes are bounded and the cache is
//! warm-up tolerant.
//!
//! ## Bounded-saturation invariant
//!
//! The doorkeeper bloom filter is a *bounded-memory* structure:
//! every set bit must eventually be cleared, otherwise after
//! roughly `capacity_bits / NUM_HASHES` distinct touches every
//! probe returns `all_set == true` and admission degenerates to
//! "admit everything", violating the second-touch policy and
//! amplifying NVMe writes.
//!
//! To prevent that, the doorkeeper is cleared on a probe-count
//! cadence sized to the bloom's bit budget (`~capacity_pages`
//! probes between clears, well below the bloom's saturation
//! point). The count-min sketch continues to age on its own
//! admit-count cadence (`~10 * capacity_pages`). Both cadences
//! are bounded by `capacity_pages`, so the filter's "memory
//! window" for first-vs-second-touch judgments stays proportional
//! to the working set the cache is sized for, per the design.

use std::sync::Mutex;

use crate::storage::types::{GOLDEN_RATIO_64, PageKey};

pub struct AdmissionFilter {
    inner: Mutex<Inner>,
    capacity_bits: u64,
    sketch_width: u32,
}

struct Inner {
    doorkeeper: Box<[u64]>,
    sketch: [Box<[u8]>; 4],
    inserts_since_sketch_age: u64,
    sketch_age_threshold: u64,
    probes_since_doorkeeper_clear: u64,
    doorkeeper_clear_threshold: u64,
}

const NUM_HASHES: u32 = 3;

// Domain tag for the doorkeeper bloom filter probes. Not a
// secret; see PageKey::mix. Sketch rows use domains 1..=4 via
// sketch_index, so 0 is reserved for the doorkeeper.
const DOORKEEPER_DOMAIN: u32 = 0;

impl AdmissionFilter {
    pub fn new(capacity_pages: u64, sketch_multiplier: u32) -> Self {
        let capacity_bits = (capacity_pages.max(1) * 8).max(64);
        let words = capacity_bits.div_ceil(64) as usize;
        let sketch_width = (capacity_pages.max(1) as u32).saturating_mul(sketch_multiplier.max(1));
        let row = vec![0u8; sketch_width as usize].into_boxed_slice();
        let capacity_pages = capacity_pages.max(1);
        Self {
            inner: Mutex::new(Inner {
                doorkeeper: vec![0u64; words].into_boxed_slice(),
                sketch: [row.clone(), row.clone(), row.clone(), row],
                inserts_since_sketch_age: 0,
                // Halve the sketch (aging) every ~10 * capacity admits.
                sketch_age_threshold: capacity_pages.saturating_mul(10),
                probes_since_doorkeeper_clear: 0,
                // Clear the doorkeeper before it saturates. With
                // `capacity_bits = 8 * capacity_pages` and
                // NUM_HASHES = 3, every ~capacity_pages distinct
                // touches set ~3 * capacity_pages bits out of
                // 8 * capacity_pages, well under the 50%-fill point
                // (~1.85 * capacity_pages keys) where false positives
                // explode. Clearing every `capacity_pages` probes
                // therefore keeps the all-set probability bounded
                // regardless of workload shape.
                doorkeeper_clear_threshold: capacity_pages,
            }),
            capacity_bits,
            sketch_width,
        }
    }

    /// Return true if `key` should be admitted to the resident
    /// set. The first call for a given key returns false and
    /// registers the key in the doorkeeper; the second call (or
    /// later, within the same epoch) returns true.
    pub fn should_admit(&self, key: &PageKey) -> bool {
        let mut inner = self.inner.lock().unwrap();
        let admit = doorkeeper_probe_and_set(&mut inner.doorkeeper, self.capacity_bits, key);
        if admit {
            sketch_bump(&mut inner.sketch, self.sketch_width, key);
            inner.inserts_since_sketch_age += 1;
            if inner.inserts_since_sketch_age >= inner.sketch_age_threshold {
                age(&mut inner.sketch);
                inner.inserts_since_sketch_age = 0;
            }
        }
        // The doorkeeper consumes bit budget on *every* probe (first
        // touches set bits but return `false`), so it must be cleared
        // on a probe-count cadence rather than an admit-count cadence.
        // Without this, a stream of one-hit-wonders saturates the
        // bloom and `should_admit` degenerates to "admit everything".
        inner.probes_since_doorkeeper_clear += 1;
        if inner.probes_since_doorkeeper_clear >= inner.doorkeeper_clear_threshold {
            clear_doorkeeper(&mut inner.doorkeeper);
            inner.probes_since_doorkeeper_clear = 0;
        }
        admit
    }

    /// Reset all state. Tests and explicit warmup boundaries
    /// call this. Restart is handled at engine construction by
    /// simply not persisting any of this state.
    pub fn reset(&self) {
        let mut inner = self.inner.lock().unwrap();
        for w in inner.doorkeeper.iter_mut() {
            *w = 0;
        }
        for row in inner.sketch.iter_mut() {
            for c in row.iter_mut() {
                *c = 0;
            }
        }
        inner.inserts_since_sketch_age = 0;
        inner.probes_since_doorkeeper_clear = 0;
    }

    /// Approximate frequency estimate from the count-min
    /// sketch. Used by callers that want to make their own
    /// decision (e.g. compare against a victim's estimate).
    pub fn frequency(&self, key: &PageKey) -> u8 {
        let inner = self.inner.lock().unwrap();
        let mut min = u8::MAX;
        for (i, row) in inner.sketch.iter().enumerate() {
            let idx = sketch_index(key, i as u32, self.sketch_width);
            min = min.min(row[idx]);
        }
        min
    }

    /// Increment the sketch without consulting the doorkeeper.
    /// Exposed so the engine can record hits as well as admits.
    pub fn record_frequency(&self, key: &PageKey) {
        let mut inner = self.inner.lock().unwrap();
        sketch_bump(&mut inner.sketch, self.sketch_width, key);
    }
}

fn doorkeeper_probe_and_set(words: &mut [u64], bits: u64, key: &PageKey) -> bool {
    let h = key.mix(DOORKEEPER_DOMAIN);
    let mut all_set = true;
    for i in 0..NUM_HASHES {
        let bit = h.wrapping_add((i as u64).wrapping_mul(GOLDEN_RATIO_64)) % bits;
        let word = (bit / 64) as usize;
        let mask = 1u64 << (bit % 64);
        if words[word] & mask == 0 {
            all_set = false;
        }
        words[word] |= mask;
    }
    all_set
}

fn sketch_bump(rows: &mut [Box<[u8]>; 4], width: u32, key: &PageKey) {
    for i in 0..4u32 {
        let idx = sketch_index(key, i, width);
        let c = &mut rows[i as usize][idx];
        if *c < u8::MAX {
            *c += 1;
        }
    }
}

fn sketch_index(key: &PageKey, row: u32, width: u32) -> usize {
    let h = key.mix(row.wrapping_add(1));
    (h % (width as u64)) as usize
}

fn age(rows: &mut [Box<[u8]>; 4]) {
    for row in rows.iter_mut() {
        for c in row.iter_mut() {
            *c >>= 1;
        }
    }
}

fn clear_doorkeeper(words: &mut [u64]) {
    for w in words.iter_mut() {
        *w = 0;
    }
}

#[cfg(test)]
mod tests;
