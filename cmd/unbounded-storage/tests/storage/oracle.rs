// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Reference model for the storage engine DST.
//!
//! The oracle is intentionally weaker than the bufferpool oracle: a
//! cache may admit, evict, or refuse a write at its discretion, so we
//! cannot assert "every successful write is later observable". What
//! we *can* assert is content-fidelity: whenever the engine reports
//! a hit, the bytes it returned must equal one of the byte patterns
//! the workload previously wrote for that `(stripe, offset)` key.
//! This catches every silent-corruption / mis-routing class of bug
//! without constraining the engine's eviction policy.

use std::cell::RefCell;
use std::collections::{HashMap, HashSet};

use unbounded_storage::bufferpool::StripeKey;

type WrittenMap = HashMap<(StripeKey, u64), HashSet<Vec<u8>>>;

#[derive(Default)]
pub struct Oracle {
    /// For each `(stripe, offset)` we record the set of byte
    /// patterns that the workload has ever submitted via
    /// `write_page`. A successful read must match at least one of
    /// them.
    written: RefCell<WrittenMap>,
}

impl Oracle {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn record_write(&self, key: StripeKey, offset: u64, bytes: Vec<u8>) {
        self.written
            .borrow_mut()
            .entry((key, offset))
            .or_default()
            .insert(bytes);
    }

    /// Returns `true` if `bytes` matches at least one previously
    /// written pattern for `(key, offset)`. Returns `false` if the
    /// key was never written (in which case the engine should not
    /// have reported a hit at all).
    pub fn allows_read(&self, key: StripeKey, offset: u64, bytes: &[u8]) -> bool {
        match self.written.borrow().get(&(key, offset)) {
            Some(set) => set.iter().any(|v| v.as_slice() == bytes),
            None => false,
        }
    }

    pub fn was_written(&self, key: StripeKey, offset: u64) -> bool {
        self.written.borrow().contains_key(&(key, offset))
    }
}
