// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Real-file setup accounting fixtures.
use super::*;

use std::sync::atomic::{AtomicUsize, Ordering};

#[path = "slab_io_setup.rs"]
mod slab_io_setup;

static NEXT: AtomicUsize = AtomicUsize::new(0);

struct Fixture(std::path::PathBuf);
impl Fixture {
    fn new() -> (Self, Allocator) {
        Self::with_size(32 * WIDE)
    }
    fn with_size(size: u64) -> (Self, Allocator) {
        let path = std::env::temp_dir().join(format!(
            "racer-allocator-setup-{}-{}",
            std::process::id(),
            NEXT.fetch_add(1, Ordering::Relaxed)
        ));
        let mut slab = Slab::create(&path, size, 1).unwrap();
        let a = Allocator::open_inner(slab.take_shard(ShardId::at(0)).unwrap(), config()).unwrap();
        (Self(path), a)
    }
}
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.0);
    }
}
fn config() -> Config {
    Config {
        max_pending_values: 4096,
        ..Config::default()
    }
}
