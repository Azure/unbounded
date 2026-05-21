// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! `BlockDevice`: the engine's view of a single NVMe namespace.
//!
//! The trait is intentionally narrow - read a page, write a page,
//! tell me your geometry. Higher layers (btree, engine) own
//! everything else (checksums, retries, eviction). Each concrete
//! impl is responsible for its own buffer-registration handshake
//! (no-op for [`MockDevice`], `IORING_REGISTER_BUFFERS` for the
//! Linux io_uring impl).

#![allow(async_fn_in_trait)]

mod mock;
mod scratch;
#[cfg(test)]
mod tests;

#[cfg(target_os = "linux")]
mod uring;

pub use mock::{MockDevice, MockDeviceConfig, MockFaultMode};
pub use scratch::{AcquireFut, ScratchPage, ScratchPool};

#[cfg(target_os = "linux")]
pub use uring::{UringBlockDevice, UringConfig};

use crate::storage::types::{Error, Lba};

/// Single-disk block device. The engine talks to exactly one of
/// these per shard.
///
/// The trait is generic over the runtime: futures are not boxed,
/// so the executor sees them as `impl Future`. This means
/// `dyn BlockDevice` is not legal - the engine is generic over the
/// device instead. That's intentional: there is no production
/// scenario where we mix device implementations within a single
/// shard at runtime.
pub trait BlockDevice {
    /// Page size in bytes, equal to the NVMe atomic write unit.
    /// Default 4096; we never split a btree page across two of
    /// these.
    fn page_size(&self) -> usize;

    /// Total addressable pages on this device. LBAs are valid for
    /// `0 <= lba < capacity_pages`.
    fn capacity_pages(&self) -> u64;

    /// Pre-register a pinned buffer with the device. The
    /// io_uring backend tracks each registration as an
    /// `IORING_REGISTER_BUFFERS` slot and matches per-I/O buffer
    /// pointers back to the appropriate slot at submission time;
    /// mocks no-op. Multiple calls accumulate: each call adds a
    /// new registered region. All buffers handed to
    /// [`Self::read`] / [`Self::write`] must lie inside one of
    /// the registered regions.
    fn register_buffers(&self, base: *mut u8, len: usize) -> Result<(), Error>;

    /// Read exactly `dst.len()` bytes from `lba * page_size` into
    /// `dst`. Resolves with `Err(Io)` on hard I/O failure; the
    /// engine collapses corruption-style failures into a cache
    /// miss at a higher layer, so this method does not validate
    /// content.
    async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), Error>;

    /// Write `src` to `lba * page_size`. Same semantics as
    /// [`BlockDevice::read`] regarding errors.
    async fn write(&self, lba: Lba, src: &[u8]) -> Result<(), Error>;

    /// Hint for how many concurrent writes the device can absorb
    /// efficiently. The mutator uses this to size its commit
    /// batches.
    fn write_queue_depth(&self) -> u32;

    /// Drive any submitted-but-not-yet-completed I/O forward. The
    /// io_uring backend pushes queued SQEs to the kernel and reaps
    /// available CQEs, waking the tasks awaiting them. Backends
    /// that complete synchronously (mocks, simulator) leave this
    /// as the default no-op.
    ///
    /// Executors should call this periodically (e.g. on idle) so
    /// in-flight ops can make progress without per-future kernel
    /// waits.
    fn progress(&self) -> Result<(), Error> {
        Ok(())
    }
}
