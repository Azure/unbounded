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
    inserts_since_reset: u64,
    reset_threshold: u64,
}

const NUM_HASHES: u32 = 3;

impl AdmissionFilter {
    pub fn new(capacity_pages: u64, sketch_multiplier: u32) -> Self {
        let capacity_bits = (capacity_pages.max(1) * 8).max(64);
        let words = capacity_bits.div_ceil(64) as usize;
        let sketch_width = (capacity_pages.max(1) as u32).saturating_mul(sketch_multiplier.max(1));
        let row = vec![0u8; sketch_width as usize].into_boxed_slice();
        Self {
            inner: Mutex::new(Inner {
                doorkeeper: vec![0u64; words].into_boxed_slice(),
                sketch: [row.clone(), row.clone(), row.clone(), row],
                inserts_since_reset: 0,
                // Halve the sketch (aging) every ~10 * capacity admits.
                reset_threshold: capacity_pages.max(1).saturating_mul(10),
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
            inner.inserts_since_reset += 1;
            if inner.inserts_since_reset >= inner.reset_threshold {
                age(&mut inner.sketch);
                inner.inserts_since_reset = 0;
            }
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
        inner.inserts_since_reset = 0;
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
    let h = key.mix(0);
    let mut all_set = true;
    for i in 0..NUM_HASHES {
        let bit =
            h.wrapping_add((i as u64).wrapping_mul(GOLDEN_RATIO_64)) % bits;
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

#[cfg(test)]
mod tests;
