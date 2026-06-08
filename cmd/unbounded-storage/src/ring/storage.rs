// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Block read/write facade over [`RingCore`].
//!
//! [`StorageRing`] is the disk-side facade: `READ_FIXED` / `WRITE_FIXED`
//! against a registered buffer table, addressing files by their
//! registered `Fixed` index. It is geometry-agnostic - callers own all
//! bounds checking and translate page indices to byte offsets - so the
//! ring only deals in raw `(file_index, offset_bytes, slice)` triples.
//!
//! In production the underlying ring is built with
//! `IOPOLL | SINGLE_ISSUER | DEFER_TASKRUN`
//! ([`StorageRingConfig::default`]); [`StorageRingConfig::test_local`]
//! strips every flag so the same path runs against a regular tmpfile.

use std::cell::RefCell;
use std::io;
use std::os::fd::RawFd;

use io_uring::{opcode, types};

use super::core::{OpFut, OpResource, RingCore, RingSetup};
use crate::storage::types::Error;

/// Knobs for building a [`StorageRing`]. The defaults are what
/// production NVMe configuration wants; [`StorageRingConfig::test_local`]
/// strips every flag that requires real hardware so the same code can be
/// exercised against a tmpfile from `cargo test`.
#[derive(Copy, Clone, Debug)]
pub struct StorageRingConfig {
    /// Set `IORING_SETUP_IOPOLL`. Requires `O_DIRECT` files and a
    /// pollable block device.
    pub iopoll: bool,
    /// Set `IORING_SETUP_SINGLE_ISSUER`.
    pub single_issuer: bool,
    /// Set `IORING_SETUP_DEFER_TASKRUN`. Pairs with `single_issuer`.
    pub defer_taskrun: bool,
    /// Submission/completion queue depth.
    pub queue_depth: u32,
}

impl Default for StorageRingConfig {
    fn default() -> Self {
        Self {
            iopoll: true,
            single_issuer: true,
            defer_taskrun: true,
            queue_depth: 256,
        }
    }
}

impl StorageRingConfig {
    /// Knob set that works against a regular file on any reasonable
    /// filesystem. Used by the inline tests here and by anyone
    /// exercising the engine without a real NVMe namespace.
    pub fn test_local() -> Self {
        Self {
            iopoll: false,
            single_issuer: false,
            defer_taskrun: false,
            queue_depth: 8,
        }
    }
}

/// Disk-side facade over a [`RingCore`]. Owns the registered file table
/// so each [`Self::register_file`] returns a stable `Fixed` index.
pub struct StorageRing {
    core: RingCore,
    /// Registered file descriptors, parallel to the kernel-side table.
    /// The Nth entry is addressed as `types::Fixed(N)`.
    files: RefCell<Vec<RawFd>>,
}

impl StorageRing {
    /// Build a storage ring per `cfg`.
    pub fn new(cfg: StorageRingConfig) -> Result<Self, Error> {
        if cfg.queue_depth == 0 {
            return Err(Error::Io(libc::EINVAL));
        }
        let setup = RingSetup {
            iopoll: cfg.iopoll,
            single_issuer: cfg.single_issuer,
            defer_taskrun: cfg.defer_taskrun,
        };
        let core = RingCore::new(cfg.queue_depth, setup).map_err(io_err_to_storage)?;
        Ok(Self {
            core,
            files: RefCell::new(Vec::new()),
        })
    }

    /// Append `fd` to the registered file table and return its `Fixed`
    /// index. Re-registers the whole table (the only form io_uring 0.6
    /// exposes); expected at bring-up, before any I/O is in flight.
    pub fn register_file(&self, fd: RawFd) -> Result<u32, Error> {
        let mut new_files = self.files.borrow().clone();
        new_files.push(fd);
        self.core
            .register_files(&new_files)
            .map_err(io_err_to_storage)?;
        let idx = (new_files.len() - 1) as u32;
        *self.files.borrow_mut() = new_files;
        Ok(idx)
    }

    /// Register `[base, base + len)` as a fixed buffer region for
    /// subsequent `READ_FIXED` / `WRITE_FIXED` ops.
    pub fn register_buffers(&self, base: *mut u8, len: usize) -> Result<(), Error> {
        self.core
            .register_buffer(base, len)
            .map(|_| ())
            .map_err(io_err_to_storage)
    }

    /// Read `dst.len()` bytes from `offset_bytes` of the registered file
    /// `file_index` into `dst`. `dst` must lie fully inside a registered
    /// buffer region.
    pub async fn read(
        &self,
        file_index: u32,
        offset_bytes: u64,
        dst: &mut [u8],
    ) -> Result<(), Error> {
        let buf_index = self
            .core
            .resolve_buf_index(dst.as_ptr(), dst.len())
            .map_err(io_err_to_storage)?;
        let ud = self.core.alloc_user_data();
        let sqe = opcode::ReadFixed::new(
            types::Fixed(file_index),
            dst.as_mut_ptr(),
            dst.len() as u32,
            buf_index,
        )
        .offset(offset_bytes)
        .build()
        .user_data(ud);
        let expected = dst.len();
        // SAFETY: `dst` lives inside the registered region resolved
        // above and the caller holds it until this future resolves.
        let slot = self
            .core
            .submit_op(sqe, false, OpResource::None)
            .await
            .map_err(io_err_to_storage)?;
        let res = OpFut::new(&self.core, ud, slot).await;
        validate_io(res, expected)
    }

    /// Write `src.len()` bytes from `src` to `offset_bytes` of the
    /// registered file `file_index`. `src` must lie fully inside a
    /// registered buffer region.
    pub async fn write(&self, file_index: u32, offset_bytes: u64, src: &[u8]) -> Result<(), Error> {
        let buf_index = self
            .core
            .resolve_buf_index(src.as_ptr(), src.len())
            .map_err(io_err_to_storage)?;
        let ud = self.core.alloc_user_data();
        let sqe = opcode::WriteFixed::new(
            types::Fixed(file_index),
            src.as_ptr(),
            src.len() as u32,
            buf_index,
        )
        .offset(offset_bytes)
        .build()
        .user_data(ud);
        let expected = src.len();
        // SAFETY: `src` lives inside the registered region resolved
        // above and the caller holds it until this future resolves.
        let slot = self
            .core
            .submit_op(sqe, false, OpResource::None)
            .await
            .map_err(io_err_to_storage)?;
        let res = OpFut::new(&self.core, ud, slot).await;
        validate_io(res, expected)
    }

    /// Push queued SQEs and reap any available CQEs.
    pub fn progress(&self) -> Result<(), Error> {
        self.core.progress().map(|_| ()).map_err(io_err_to_storage)
    }

    /// Configured submission/completion depth.
    pub fn queue_depth(&self) -> u32 {
        self.core.queue_depth()
    }

    /// Number of live (submitted, not yet reaped) ops. Used by the
    /// back-pressure tests to assert the queue-depth bound and by the
    /// disk supervisor to confirm all I/O has drained before tearing
    /// down a storage core.
    pub(crate) fn in_flight(&self) -> u32 {
        self.core.in_flight()
    }

    /// High-water mark of in-flight ops since this ring was created.
    /// Used by the back-pressure tests to confirm ops actually went
    /// in-flight without racing the per-poll completion path.
    pub(crate) fn peak_in_flight(&self) -> u32 {
        self.core.peak_in_flight()
    }
}

/// Validate a fixed-I/O CQE `res` against the expected byte count.
fn validate_io(res: i32, expected: usize) -> Result<(), Error> {
    if res < 0 {
        return Err(Error::Io(-res));
    }
    if res as usize != expected {
        return Err(Error::Io(libc::EIO));
    }
    Ok(())
}

fn io_err_to_storage(e: io::Error) -> Error {
    Error::Io(e.raw_os_error().unwrap_or(libc::EIO))
}

#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::io::Write as _;
    use std::os::fd::AsRawFd;
    use std::path::PathBuf;
    use std::pin::Pin;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::task::{Context, Poll};

    use super::*;
    use crate::runtime::noop_waker;

    static SEQ: AtomicU64 = AtomicU64::new(0);

    /// Drive multiple futures cooperatively, calling `ring.progress()`
    /// between polls. Returns the outputs in submission order.
    fn pump<O>(ring: &StorageRing, mut futs: Vec<Pin<Box<dyn Future<Output = O> + '_>>>) -> Vec<O> {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut out: Vec<Option<O>> = (0..futs.len()).map(|_| None).collect();
        let mut spins = 0u32;
        loop {
            let mut made_progress = false;
            for (i, fut) in futs.iter_mut().enumerate() {
                if out[i].is_some() {
                    continue;
                }
                if let Poll::Ready(v) = Pin::as_mut(fut).poll(&mut cx) {
                    out[i] = Some(v);
                    made_progress = true;
                }
            }
            ring.progress().expect("progress");
            if out.iter().all(|o| o.is_some()) {
                return out.into_iter().map(|o| o.unwrap()).collect();
            }
            if !made_progress {
                spins += 1;
                assert!(spins < 1_000_000, "pump spun without progress");
            } else {
                spins = 0;
            }
        }
    }

    struct TempPath(PathBuf);
    impl Drop for TempPath {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.0);
        }
    }

    /// Create a tmpfile seeded with `pages` pages of `fill`, returning an
    /// open `O_RDWR` `File` (no O_DIRECT) plus its path guard.
    fn make_tempfile(pages: u64, page_size: usize, fill: u8) -> (std::fs::File, TempPath) {
        let n = SEQ.fetch_add(1, Ordering::Relaxed);
        let mut p = std::env::temp_dir();
        p.push(format!("ring-storage-{}-{}.bin", std::process::id(), n));
        {
            let mut f = std::fs::OpenOptions::new()
                .create(true)
                .truncate(true)
                .write(true)
                .open(&p)
                .expect("create tempfile");
            let block = vec![fill; page_size];
            for _ in 0..pages {
                f.write_all(&block).expect("seed tempfile");
            }
            f.sync_all().expect("sync tempfile");
        }
        let f = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(&p)
            .expect("reopen tempfile");
        (f, TempPath(p))
    }

    fn aligned_buffer(len: usize) -> (Vec<u8>, *mut u8) {
        let words = len.div_ceil(8);
        let mut v = vec![0u8; words * 8 + 4096];
        let raw = v.as_mut_ptr();
        let aligned_off = raw.align_offset(4096);
        let base = unsafe { raw.add(aligned_off) };
        (v, base)
    }

    #[test]
    fn concurrent_reads_and_write_readback() {
        const PAGE: usize = 4096;
        const PAGES: u64 = 16;
        let (file, _guard) = make_tempfile(PAGES, PAGE, 0xa5);
        let ring = StorageRing::new(StorageRingConfig::test_local()).expect("ring");
        let fidx = ring.register_file(file.as_raw_fd()).expect("register file");
        assert_eq!(fidx, 0);

        // One registered buffer big enough for 8 read slices + 1 write
        // slice + 1 read-back slice.
        let buf_len = PAGE * 10;
        let (_owner, base) = aligned_buffer(buf_len);
        ring.register_buffers(base, buf_len).unwrap();

        // 8 concurrent reads of the first 8 pages into disjoint slices.
        let mut futs: Vec<Pin<Box<dyn Future<Output = Result<(), Error>> + '_>>> = Vec::new();
        let mut dst_ptrs = Vec::new();
        for i in 0..8u64 {
            let ptr = unsafe { base.add((i as usize) * PAGE) };
            dst_ptrs.push(ptr);
            let slice = unsafe { std::slice::from_raw_parts_mut(ptr, PAGE) };
            slice.fill(0);
            futs.push(Box::pin(ring.read(fidx, i * PAGE as u64, slice)));
        }
        let results = pump(&ring, futs);
        for r in &results {
            r.as_ref().expect("read ok");
        }
        for ptr in &dst_ptrs {
            let slice = unsafe { std::slice::from_raw_parts(*ptr, PAGE) };
            assert!(slice.iter().all(|b| *b == 0xa5), "page seeded 0xa5");
        }

        // Write a distinctive page to LBA 12, then read it back.
        let write_ptr = unsafe { base.add(8 * PAGE) };
        let read_ptr = unsafe { base.add(9 * PAGE) };
        {
            let wslice = unsafe { std::slice::from_raw_parts_mut(write_ptr, PAGE) };
            for (i, b) in wslice.iter_mut().enumerate() {
                *b = (i as u8).wrapping_mul(31).wrapping_add(3);
            }
        }
        let offset = 12 * PAGE as u64;
        {
            let wslice = unsafe { std::slice::from_raw_parts(write_ptr, PAGE) };
            let futs: Vec<Pin<Box<dyn Future<Output = Result<(), Error>> + '_>>> =
                vec![Box::pin(ring.write(fidx, offset, wslice))];
            for r in pump(&ring, futs) {
                r.expect("write ok");
            }
        }
        {
            let rslice = unsafe { std::slice::from_raw_parts_mut(read_ptr, PAGE) };
            rslice.fill(0);
            let futs: Vec<Pin<Box<dyn Future<Output = Result<(), Error>> + '_>>> =
                vec![Box::pin(ring.read(fidx, offset, rslice))];
            for r in pump(&ring, futs) {
                r.expect("readback ok");
            }
        }
        let w = unsafe { std::slice::from_raw_parts(write_ptr, PAGE) };
        let r = unsafe { std::slice::from_raw_parts(read_ptr, PAGE) };
        assert_eq!(w, r, "read-back matches written bytes");
    }

    #[test]
    fn submit_backpressure_parks_and_resumes() {
        const PAGE: usize = 4096;
        const PAGES: u64 = 16;
        const QD: u32 = 4;
        let (file, _guard) = make_tempfile(PAGES, PAGE, 0);
        let cfg = StorageRingConfig {
            queue_depth: QD,
            ..StorageRingConfig::test_local()
        };
        let ring = StorageRing::new(cfg).expect("ring");
        let fidx = ring.register_file(file.as_raw_fd()).expect("register file");

        // A single page-sized buffer shared by all writes (write-only
        // semantics don't need unique source bytes for this test).
        let buf_len = PAGE;
        let (_owner, base) = aligned_buffer(buf_len);
        ring.register_buffers(base, buf_len).unwrap();
        let src = unsafe { std::slice::from_raw_parts_mut(base, PAGE) };
        for (i, b) in src.iter_mut().enumerate() {
            *b = (i as u8).wrapping_mul(7);
        }
        let src_const: &[u8] = unsafe { std::slice::from_raw_parts(base, PAGE) };

        let mut futs: Vec<Pin<Box<dyn Future<Output = Result<(), Error>> + '_>>> =
            Vec::with_capacity(16);
        for i in 0..16u64 {
            futs.push(Box::pin(ring.write(fidx, i * PAGE as u64, src_const)));
        }

        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut out: Vec<Option<Result<(), Error>>> = (0..futs.len()).map(|_| None).collect();
        let mut spins = 0u32;
        loop {
            let mut made_progress = false;
            for (i, fut) in futs.iter_mut().enumerate() {
                if out[i].is_some() {
                    continue;
                }
                if let Poll::Ready(v) = Pin::as_mut(fut).poll(&mut cx) {
                    out[i] = Some(v);
                    made_progress = true;
                }
                assert!(
                    ring.in_flight() <= QD,
                    "in_flight={} exceeds queue_depth={QD}",
                    ring.in_flight(),
                );
            }
            ring.progress().expect("progress");
            assert!(ring.in_flight() <= QD);
            if out.iter().all(|o| o.is_some()) {
                break;
            }
            if !made_progress {
                spins += 1;
                assert!(spins < 1_000_000, "pump spun without progress");
            } else {
                spins = 0;
            }
        }
        for r in &out {
            r.as_ref().unwrap().as_ref().expect("write ok");
        }
        // The per-poll assertions above prove the ceiling held. The
        // peak high-water mark is read from the ring rather than sampled
        // externally: with buffered I/O each write can complete within
        // its own poll, returning `in_flight` to zero before this loop
        // ever samples it, so an external sample can legitimately never
        // observe a non-zero in-flight count.
        let peak = ring.peak_in_flight();
        assert!(peak > 0, "should have had in-flight ops");
        assert!(peak <= QD);
    }
}
