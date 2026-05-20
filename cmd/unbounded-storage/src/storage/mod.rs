// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-disk storage engine: btree index over a CoW NVMe layout with
//! an in-memory cache, eviction policy, singleflight, admission
//! filter, and an io_uring-backed block device. See
//! `designs/full-storage-design.md` for the overall architecture.
//!
//! Composition is bottom-up: [`blockdev`] is the only thing that
//! touches a kernel device, [`alloc`] and [`refcount`] are pure
//! tables over LBA space, [`btree`] is a CoW B+tree mapped on top of
//! a [`blockdev::BlockDevice`], [`singleflight`], [`lru`], and
//! [`admission`] are concurrency / policy primitives on the cache
//! side, and finally [`engine`] wires everything into the
//! `bufferpool::BlockStore` surface the rest of the project sees.

pub mod admission;
pub mod alloc;
pub mod blockdev;
pub mod btree;
pub mod engine;
pub mod local;
pub mod lru;
pub mod refcount;
pub mod singleflight;
pub mod traits;
pub mod types;

pub use engine::{EngineConfig, StorageEngine};
pub use local::{LocalStorage, ShardLocalStore};
pub use traits::PageChecksum;
pub use types::{Checksum, DiskId, Error, Lba, PageKey};
