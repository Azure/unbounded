// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Shared io_uring engine and its facades.
//!
//! [`RingCore`](core::RingCore) is the single low-level engine; the two
//! facades over it are [`StorageRing`] (block read/write, IOPOLL-capable)
//! and [`NetworkRing`] (sockets, no IOPOLL). [`set_current_storage_ring`]
//! and friends expose a thread-local registry of the current shard's
//! [`StorageRing`].

mod core;
mod network;
mod registry;
mod storage;

pub use network::{NetHandle, NetworkRing, SockAddr};
pub use registry::{
    clear_current_storage_ring, current_storage_ring, set_current_storage_ring,
    with_current_storage_ring,
};
pub use storage::{StorageRing, StorageRingConfig};
