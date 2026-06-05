// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! `BlockDevice` backed by the current storage core's [`StorageRing`].
//!
//! A [`CoreLocalDevice`] owns no ring at all. It carries only the small,
//! copyable disk metadata (page size, capacity, write queue depth) plus
//! the registered `Fixed` file index for its disk, and resolves the
//! actual ring at call time from the thread-local registry
//! ([`crate::ring`]). That makes it trivially `Send + Sync`:
//! the engine that holds it can be published to any thread, but every
//! [`BlockDevice`] call only succeeds on the storage core whose ring
//! has been installed (via
//! [`set_current_storage_ring`](crate::ring::set_current_storage_ring)).
//! Off-core calls fail with `Err(Io(ENXIO))` rather than touching a
//! foreign ring.

use crate::ring::{current_storage_ring, with_current_storage_ring};
use crate::storage::blockdev::BlockDevice;
use crate::storage::types::{Error, Lba};

/// `Send + Sync` [`BlockDevice`] that delegates every I/O to the
/// current thread's [`StorageRing`](crate::ring::StorageRing).
///
/// The device holds no ring handle of its own; it stores the disk
/// geometry and the ring-side `Fixed` file index assigned by
/// [`StorageRing::register_file`](crate::ring::StorageRing::register_file)
/// when the storage core opened the disk. Reads and writes translate a
/// page-indexed [`Lba`] into a byte offset (`lba * page_size`) and hand
/// the `(file_index, offset, slice)` triple to the ring.
#[derive(Copy, Clone, Debug)]
pub struct CoreLocalDevice {
    /// Registered `Fixed` file index for this disk on the storage
    /// core's ring.
    file_index: u32,
    page_size: usize,
    capacity_pages: u64,
    write_queue_depth: u32,
}

impl CoreLocalDevice {
    /// Build a device bound to `file_index` on the storage core's
    /// ring, carrying the disk geometry the engine needs without
    /// touching the ring.
    pub fn new(
        file_index: u32,
        page_size: usize,
        capacity_pages: u64,
        write_queue_depth: u32,
    ) -> Self {
        Self {
            file_index,
            page_size,
            capacity_pages,
            write_queue_depth,
        }
    }

    /// The ring-side `Fixed` file index assigned at open time.
    pub fn file_index(&self) -> u32 {
        self.file_index
    }

    /// Validate an I/O against the device geometry, returning the
    /// byte offset of `lba` on success. Mirrors the bounds checks the
    /// io_uring backend performs so callers see identical error
    /// semantics regardless of which device backs the engine.
    fn io_offset(&self, lba: Lba, len: usize) -> Result<u64, Error> {
        if len == 0 || len % self.page_size != 0 {
            return Err(Error::Io(libc::EINVAL));
        }
        let n_pages = (len / self.page_size) as u64;
        if lba
            .0
            .checked_add(n_pages)
            .is_none_or(|end| end > self.capacity_pages)
        {
            return Err(Error::OutOfRange);
        }
        Ok(byte_offset(lba, self.page_size))
    }
}

impl BlockDevice for CoreLocalDevice {
    fn page_size(&self) -> usize {
        self.page_size
    }

    fn capacity_pages(&self) -> u64 {
        self.capacity_pages
    }

    fn register_buffers(&self, base: *mut u8, len: usize) -> Result<(), Error> {
        with_current_storage_ring(|ring| ring.register_buffers(base, len)).unwrap_or_else(off_core)
    }

    async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), Error> {
        let offset = self.io_offset(lba, dst.len())?;
        let ring = current_storage_ring().ok_or_else(off_core_err)?;
        ring.read(self.file_index, offset, dst).await
    }

    async fn write(&self, lba: Lba, src: &[u8]) -> Result<(), Error> {
        let offset = self.io_offset(lba, src.len())?;
        let ring = current_storage_ring().ok_or_else(off_core_err)?;
        ring.write(self.file_index, offset, src).await
    }

    fn write_queue_depth(&self) -> u32 {
        self.write_queue_depth
    }

    fn progress(&self) -> Result<(), Error> {
        with_current_storage_ring(|ring| ring.progress()).unwrap_or_else(off_core)
    }
}

/// Byte offset of `lba` for a device whose page size is `page_size`.
/// The ring is geometry-agnostic, so the device owns this conversion.
fn byte_offset(lba: Lba, page_size: usize) -> u64 {
    lba.0.saturating_mul(page_size as u64)
}

/// Error returned when a [`CoreLocalDevice`] call lands on a thread
/// that has no storage ring installed (i.e. not a storage core).
/// `ENXIO` ("no such device or address") is the closest POSIX match.
fn off_core() -> Result<(), Error> {
    Err(off_core_err())
}

fn off_core_err() -> Error {
    Error::Io(libc::ENXIO)
}

#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::pin::Pin;
    use std::task::{Context, Poll};

    use super::*;
    use crate::ring::clear_current_storage_ring;
    use crate::runtime::noop_waker;

    fn block_on<F: Future>(mut fut: F) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        // SAFETY: `fut` is owned on the stack and never moved after
        // pinning; this mirrors the noop-waker block_on used
        // elsewhere in the crate.
        let mut fut = unsafe { Pin::new_unchecked(&mut fut) };
        for _ in 0..1_000_000 {
            if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
                return v;
            }
            std::thread::yield_now();
        }
        panic!("block_on: future did not complete within spin budget");
    }

    #[test]
    fn byte_offset_scales_lba_by_page_size() {
        assert_eq!(byte_offset(Lba(0), 4096), 0);
        assert_eq!(byte_offset(Lba(1), 4096), 4096);
        assert_eq!(byte_offset(Lba(10), 4096), 40_960);
        // A 2 MiB page size is the engine's default cache page.
        assert_eq!(byte_offset(Lba(3), 2 * 1024 * 1024), 3 * 2 * 1024 * 1024);
    }

    #[test]
    fn io_offset_validates_geometry() {
        let dev = CoreLocalDevice::new(0, 4096, 16, 8);
        // Happy path: page-aligned, in range.
        assert_eq!(dev.io_offset(Lba(2), 4096).unwrap(), 8192);
        // Non-multiple of page size.
        assert!(matches!(
            dev.io_offset(Lba(0), 4097),
            Err(Error::Io(n)) if n == libc::EINVAL
        ));
        // Zero length.
        assert!(matches!(
            dev.io_offset(Lba(0), 0),
            Err(Error::Io(n)) if n == libc::EINVAL
        ));
        // Past capacity (page 16 does not exist; valid range 0..16).
        assert!(matches!(
            dev.io_offset(Lba(16), 4096),
            Err(Error::OutOfRange)
        ));
        // Multi-page run that overruns capacity.
        assert!(matches!(
            dev.io_offset(Lba(15), 2 * 4096),
            Err(Error::OutOfRange)
        ));
    }

    #[test]
    fn read_off_core_returns_enxio() {
        // Ensure no ring is installed on this test thread.
        clear_current_storage_ring();
        let dev = CoreLocalDevice::new(0, 4096, 16, 8);
        let mut buf = vec![0u8; 4096];
        let res = block_on(dev.read(Lba(0), &mut buf));
        assert!(
            matches!(res, Err(Error::Io(n)) if n == libc::ENXIO),
            "expected ENXIO off-core, got {res:?}",
        );
    }

    #[test]
    fn write_off_core_returns_enxio() {
        clear_current_storage_ring();
        let dev = CoreLocalDevice::new(0, 4096, 16, 8);
        let buf = vec![0u8; 4096];
        let res = block_on(dev.write(Lba(0), &buf));
        assert!(
            matches!(res, Err(Error::Io(n)) if n == libc::ENXIO),
            "expected ENXIO off-core, got {res:?}",
        );
    }

    #[test]
    fn register_and_progress_off_core_return_enxio() {
        clear_current_storage_ring();
        let dev = CoreLocalDevice::new(0, 4096, 16, 8);
        let mut buf = vec![0u8; 4096];
        assert!(matches!(
            dev.register_buffers(buf.as_mut_ptr(), buf.len()),
            Err(Error::Io(n)) if n == libc::ENXIO
        ));
        assert!(matches!(
            BlockDevice::progress(&dev),
            Err(Error::Io(n)) if n == libc::ENXIO
        ));
    }

    #[test]
    fn metadata_accessors_do_not_touch_the_ring() {
        clear_current_storage_ring();
        let dev = CoreLocalDevice::new(3, 4096, 128, 32);
        assert_eq!(dev.file_index(), 3);
        assert_eq!(dev.page_size(), 4096);
        assert_eq!(dev.capacity_pages(), 128);
        assert_eq!(dev.write_queue_depth(), 32);
    }

    #[test]
    fn core_local_device_is_send_and_sync() {
        fn assert_send_sync<T: Send + Sync>() {}
        assert_send_sync::<CoreLocalDevice>();
    }
}
