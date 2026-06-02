// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Cross-core `PageChannel` DST: drives the full shard-to-storage-core
//! page path on the single-threaded seeded executor.
//!
//! The path under test is client-task -> [`PageChannel::read_page`] /
//! [`PageChannel::write_page`] -> [`PageService`] -> [`StorageEngine`]
//! -> [`SimBlockDevice`]. It reuses the storage DST's sim-aware block
//! device (`crate::storage::mocks::SimBlockDevice`) so latency and
//! fault injection flow through the same framework PRNG.
//!
//! Unlike the storage DST, which drives the engine directly through
//! `LocalStorage`, this harness routes every page op over a real
//! [`PageChannel`] and executes it on a cooperative "storage-core"
//! task that interleaves `engine.run_mutator()` with
//! `PageService::poll_once`. That is the production data path the disk
//! supervisor wires up; here it runs entirely on the DST executor.
//!
//! [`PageChannel`]: unbounded_storage::storage::PageChannel
//! [`PageChannel::read_page`]: unbounded_storage::storage::PageChannel::read_page
//! [`PageChannel::write_page`]: unbounded_storage::storage::PageChannel::write_page
//! [`PageService`]: unbounded_storage::storage::PageService
//! [`StorageEngine`]: unbounded_storage::storage::StorageEngine

#![allow(clippy::arc_with_non_send_sync)]

pub mod tests;
pub mod workload;
