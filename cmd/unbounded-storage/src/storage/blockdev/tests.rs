// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![cfg(test)]

use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll};

use super::{BlockDevice, MockDevice, MockDeviceConfig, MockFaultMode};
use crate::runtime::noop_waker;
use crate::storage::types::{Error, Lba};

// Minimal noop-waker block_on so we can exercise async fns without
// pulling in tokio. Matches the pattern documented in the crate
// AGENTS.md.
fn block_on<F: Future>(fut: F) -> F::Output {
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut fut = Box::pin(fut);
    let mut spins = 0u32;
    loop {
        match Pin::as_mut(&mut fut).poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                spins += 1;
                assert!(spins < 1_000_000, "block_on spun without progress");
            }
        }
    }
}

#[test]
fn read_back_what_we_wrote() {
    let dev = MockDevice::new(MockDeviceConfig {
        page_size: 64,
        capacity_pages: 4,
        ..Default::default()
    });
    let src = vec![0xabu8; 64];
    block_on(dev.write(Lba(2), &src)).unwrap();
    let mut dst = vec![0u8; 64];
    block_on(dev.read(Lba(2), &mut dst)).unwrap();
    assert_eq!(dst, src);
    assert_eq!(dev.reads(), 1);
    assert_eq!(dev.writes(), 1);
}

#[test]
fn read_io_fault_returns_err() {
    let dev = MockDevice::new(MockDeviceConfig::default());
    dev.set_fault_mode(MockFaultMode::ReadIo);
    let mut dst = vec![0u8; 4096];
    let err = block_on(dev.read(Lba(0), &mut dst));
    assert!(matches!(err, Err(Error::Io(_))));
}

#[test]
fn read_corruption_flips_byte() {
    let dev = MockDevice::new(MockDeviceConfig {
        page_size: 16,
        capacity_pages: 2,
        ..Default::default()
    });
    let src = vec![0x11u8; 16];
    block_on(dev.write(Lba(0), &src)).unwrap();
    dev.set_fault_mode(MockFaultMode::ReadCorrupt);
    let mut dst = vec![0u8; 16];
    block_on(dev.read(Lba(0), &mut dst)).unwrap();
    assert_ne!(dst[0], src[0]);
    assert_eq!(&dst[1..], &src[1..]);
}

#[test]
fn out_of_range_lba_errors() {
    let dev = MockDevice::new(MockDeviceConfig {
        page_size: 16,
        capacity_pages: 2,
        ..Default::default()
    });
    let mut dst = vec![0u8; 16];
    assert!(matches!(
        block_on(dev.read(Lba(5), &mut dst)),
        Err(Error::OutOfRange)
    ));
    assert!(matches!(
        block_on(dev.write(Lba(5), &dst)),
        Err(Error::OutOfRange)
    ));
}

#[test]
fn register_buffers_records_handle() {
    let dev = MockDevice::new(MockDeviceConfig::default());
    let mut buf = vec![0u8; 4096];
    dev.register_buffers(buf.as_mut_ptr(), buf.len()).unwrap();
    assert_eq!(dev.registered_base(), Some(buf.as_mut_ptr()));
    assert_eq!(dev.registered_len(), buf.len());
}

#[test]
fn peek_poke_match_read_write() {
    let dev = MockDevice::new(MockDeviceConfig {
        page_size: 8,
        capacity_pages: 4,
        ..Default::default()
    });
    dev.poke(Lba(1), &[1, 2, 3, 4, 5, 6, 7, 8]);
    let mut buf = [0u8; 8];
    block_on(dev.read(Lba(1), &mut buf)).unwrap();
    assert_eq!(buf, [1, 2, 3, 4, 5, 6, 7, 8]);
    block_on(dev.write(Lba(2), &[9, 9, 9, 9, 9, 9, 9, 9])).unwrap();
    let mut peeked = [0u8; 8];
    dev.peek(Lba(2), &mut peeked);
    assert_eq!(peeked, [9, 9, 9, 9, 9, 9, 9, 9]);
}

#[cfg(target_os = "linux")]
mod uring_tests {
    use std::future::Future;
    use std::io::Write as _;
    use std::path::{Path, PathBuf};
    use std::pin::Pin;
    use std::rc::Rc;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::task::{Context, Poll};

    use super::super::{BlockDevice, CoreLocalDevice, OpenDisk, UringDevice};
    use crate::ring::{
        StorageRing, StorageRingConfig, clear_current_storage_ring, set_current_storage_ring,
    };
    use crate::runtime::noop_waker;
    use crate::storage::types::{Error, Lba};

    static SEQ: AtomicU64 = AtomicU64::new(0);

    /// Create a fresh, sized tempfile under `$TMPDIR` populated with
    /// `pages * page_size` bytes of `0xa5`.
    fn make_tempfile(pages: u64, page_size: usize) -> PathBuf {
        let n = SEQ.fetch_add(1, Ordering::Relaxed);
        let mut p = std::env::temp_dir();
        p.push(format!("uring-blockdev-{}-{}.bin", std::process::id(), n));
        let mut f = std::fs::OpenOptions::new()
            .create(true)
            .truncate(true)
            .write(true)
            .open(&p)
            .expect("create tempfile");
        let block = vec![0xa5u8; page_size];
        for _ in 0..pages {
            f.write_all(&block).expect("seed tempfile");
        }
        f.sync_all().expect("sync tempfile");
        p
    }

    struct TempPath(PathBuf);
    impl Drop for TempPath {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.0);
        }
    }

    /// RAII guard that keeps a [`StorageRing`] installed as this thread's
    /// storage ring for the body of a test and tears it down in the
    /// right order on drop.
    ///
    /// A [`CoreLocalDevice`] resolves its ring from the thread-local
    /// registry, so a ring must be installed before any device I/O. On
    /// drop the thread-local is cleared first (so a later test reusing
    /// this pooled thread never observes a stale ring), then `ring`
    /// drops - unregistering the disk fd - and finally `_file` closes
    /// it.
    struct Installed {
        ring: Rc<StorageRing>,
        _file: std::fs::File,
    }

    impl Drop for Installed {
        fn drop(&mut self) {
            clear_current_storage_ring();
        }
    }

    /// Open `path` through the production [`UringDevice::open`] path with
    /// the tmpfile-friendly `test_local` ring config (no IOPOLL, no
    /// O_DIRECT), install the returned ring, and hand back the
    /// [`CoreLocalDevice`] plus the install guard.
    fn open_installed(path: &Path, page_size: usize) -> (CoreLocalDevice, Installed) {
        let OpenDisk { device, ring, file } =
            UringDevice::open(path, StorageRingConfig::test_local(), false, page_size)
                .expect("open uring device");
        let ring = Rc::new(ring);
        set_current_storage_ring(ring.clone());
        (device, Installed { ring, _file: file })
    }

    /// Drive a single block-device future to completion, pumping the
    /// installed ring's `progress()` between polls so submitted SQEs are
    /// reaped (mirrors the production storage-core loop).
    fn run_io<O>(installed: &Installed, fut: impl Future<Output = O>) -> O {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut fut = Box::pin(fut);
        let mut spins = 0u32;
        loop {
            if let Poll::Ready(v) = Pin::as_mut(&mut fut).poll(&mut cx) {
                return v;
            }
            installed.ring.progress().expect("ring progress");
            spins += 1;
            assert!(spins < 1_000_000, "run_io spun without progress");
        }
    }

    fn aligned_buffer(len: usize) -> (Vec<u8>, *mut u8) {
        // Page-aligned via an oversized Vec we offset into.
        let words = len.div_ceil(8);
        let mut v = vec![0u8; words * 8 + 4096];
        let raw = v.as_mut_ptr();
        let aligned_off = raw.align_offset(4096);
        let base = unsafe { raw.add(aligned_off) };
        (v, base)
    }

    fn geometry() -> (usize, u64) {
        (4096, 8)
    }

    #[test]
    fn open_reports_capacity() {
        let (page_size, pages) = geometry();
        let path = TempPath(make_tempfile(pages, page_size));
        let (device, _installed) = open_installed(&path.0, page_size);
        assert_eq!(device.page_size(), page_size);
        assert_eq!(device.capacity_pages(), pages);
        assert_eq!(device.write_queue_depth(), 8);
    }

    #[test]
    fn read_back_what_we_wrote() {
        let (page_size, pages) = geometry();
        let path = TempPath(make_tempfile(pages, page_size));
        let (device, installed) = open_installed(&path.0, page_size);

        let buf_len = page_size * 4;
        let (_owner, base) = aligned_buffer(buf_len);
        device
            .register_buffers(base, buf_len)
            .expect("register buffers");

        // SAFETY: we own the buffer for the duration of the test.
        let src = unsafe { std::slice::from_raw_parts_mut(base, page_size) };
        for (i, b) in src.iter_mut().enumerate() {
            *b = (i as u8).wrapping_mul(31).wrapping_add(7);
        }
        run_io(&installed, device.write(Lba(3), src)).expect("write");

        let dst_ptr = unsafe { base.add(page_size * 2) };
        let dst = unsafe { std::slice::from_raw_parts_mut(dst_ptr, page_size) };
        dst.fill(0);
        run_io(&installed, device.read(Lba(3), dst)).expect("read");

        for (i, b) in dst.iter().enumerate() {
            assert_eq!(*b, (i as u8).wrapping_mul(31).wrapping_add(7), "byte {i}");
        }
    }

    #[test]
    fn read_unwritten_page_returns_seed_bytes() {
        let (page_size, pages) = geometry();
        let path = TempPath(make_tempfile(pages, page_size));
        let (device, installed) = open_installed(&path.0, page_size);

        let buf_len = page_size;
        let (_owner, base) = aligned_buffer(buf_len);
        device.register_buffers(base, buf_len).unwrap();

        let dst = unsafe { std::slice::from_raw_parts_mut(base, page_size) };
        dst.fill(0);
        run_io(&installed, device.read(Lba(0), dst)).expect("read");
        // make_tempfile seeds the file with 0xa5.
        assert!(dst.iter().all(|b| *b == 0xa5));
    }

    #[test]
    fn out_of_range_lba_rejected_without_io() {
        let (page_size, pages) = geometry();
        let path = TempPath(make_tempfile(pages, page_size));
        let (device, installed) = open_installed(&path.0, page_size);

        let buf_len = page_size;
        let (_owner, base) = aligned_buffer(buf_len);
        device.register_buffers(base, buf_len).unwrap();
        let dst = unsafe { std::slice::from_raw_parts_mut(base, page_size) };
        let err = run_io(&installed, device.read(Lba(pages), dst));
        assert!(matches!(err, Err(Error::OutOfRange)));
    }

    #[test]
    fn write_without_registered_buffer_errors() {
        let (page_size, pages) = geometry();
        let path = TempPath(make_tempfile(pages, page_size));
        let (device, installed) = open_installed(&path.0, page_size);

        let (_owner, base) = aligned_buffer(page_size);
        let src = unsafe { std::slice::from_raw_parts(base, page_size) };
        let err = run_io(&installed, device.write(Lba(0), src));
        assert!(matches!(err, Err(Error::Io(_))));
    }
}
