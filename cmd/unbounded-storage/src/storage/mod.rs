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
pub mod blockdev;
#[cfg(target_os = "linux")]
pub mod discovery;
pub mod disks;
pub mod types;

mod alloc;
mod btree;
mod checksum;
mod completion;
mod engine;
mod local;
mod lru;
mod metadata;
mod mutator;
mod origin;
mod page_channel;
mod refcount;
mod singleflight;
mod synthetic;

pub use admission::{AdmissionFilter, AdmitDecision, StripeAdmission};
pub use engine::{EngineConfig, StorageEngine};
pub use local::{LocalStorage, ShardLocalStore, disk_for};
pub use metadata::ObjectMetadata;
pub use origin::{METADATA_STRIPE_IDX, OriginRef, StripeReq, stripe_key};
pub use page_channel::{PageChannel, PageChannelReceiver, PageCommand, PageService, ReplySlot};
pub use synthetic::{
    SyntheticObjectId, byte_at, fill_pages as fill_synthetic_pages,
    matches_bytes as synthetic_matches_bytes, object_hash, object_id as synthetic_object_id,
    parse_object_id as parse_synthetic_object_id, splitmix64,
};
pub use types::{Checksum, DiskId, Error, Lba, PageKey};
