// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Linux io_uring backend for [`BlockDevice`].
//!
//! Each [`UringBlockDevice`] owns exactly one ring and the file
//! descriptor for one disk. The ring is set up with
//! `IORING_SETUP_SINGLE_ISSUER | IORING_SETUP_DEFER_TASKRUN |
//! IORING_SETUP_IOPOLL` in production so submission and completion
//! reaping both run on a single pinned shard thread; the device is
//! therefore `!Send + !Sync` by construction.
//!
//! All I/O is issued via `READ_FIXED` / `WRITE_FIXED` against a
//! single pre-registered buffer (`IORING_REGISTER_BUFFERS`, index 0)
//! and a single pre-registered file (`IORING_REGISTER_FILES`,
//! `Fixed(0)`), which is the configuration the design targets.
//!
//! Tests and any non-NVMe usage use [`UringConfig::test_local`]
//! which disables `IOPOLL`, `SINGLE_ISSUER`, `DEFER_TASKRUN`, and
//! `O_DIRECT` so the same code path runs on a regular tmpfile.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::ffi::CString;
use std::fs::File;
use std::io;
use std::marker::PhantomData;
use std::os::fd::{AsRawFd, FromRawFd, RawFd};
use std::path::Path;
use std::ptr::NonNull;

use io_uring::{IoUring, opcode, types};

use super::BlockDevice;
use crate::storage::types::{Error, Lba};

/// Knobs for opening a [`UringBlockDevice`]. The defaults are what
/// production NVMe configuration wants; [`UringConfig::test_local`]
/// strips out every flag that requires real hardware so the same
/// code can be exercised against a tmpfile from `cargo test`.
#[derive(Copy, Clone, Debug)]
pub struct UringConfig {
    /// Set `IORING_SETUP_IOPOLL`. Requires `o_direct == true` and a
    /// block device that supports polling.
    pub iopoll: bool,
    /// Set `IORING_SETUP_SINGLE_ISSUER`. Requires the same thread to
    /// own submit + reap for the lifetime of the ring.
    pub single_issuer: bool,
    /// Set `IORING_SETUP_DEFER_TASKRUN`. Pairs with `single_issuer`.
    pub defer_taskrun: bool,
    /// Open the underlying file with `O_DIRECT`. Mandatory for
    /// `iopoll` and recommended whenever the page cache is not in
    /// the data path.
    pub o_direct: bool,
    /// Submission queue depth (the same depth is used for the
    /// completion queue by the kernel).
    pub queue_depth: u32,
    /// Block-device page size. Must match the engine's
    /// `EngineConfig::page_size_bytes` for the disk-side path and
    /// the value used to compute LBAs.
    pub page_size: usize,
}

impl Default for UringConfig {
    fn default() -> Self {
        Self {
            iopoll: true,
            single_issuer: true,
            defer_taskrun: true,
            o_direct: true,
            queue_depth: 256,
            page_size: 4096,
        }
    }
}

impl UringConfig {
    /// Knob set that works against a regular file on any reasonable
    /// filesystem. Used by the inline tests in this file and by
    /// anyone exercising the engine without a real NVMe namespace.
    pub fn test_local() -> Self {
        Self {
            iopoll: false,
            single_issuer: false,
            defer_taskrun: false,
            o_direct: false,
            queue_depth: 8,
            page_size: 4096,
        }
    }
}

/// One ring + one file descriptor + one registered buffer. Pinned
/// to the thread that opened it; `PhantomData<*const ()>` makes
/// that non-negotiable to the type system.
pub struct UringBlockDevice {
    file: File,
    ring: RefCell<IoUring>,
    /// Pre-registered I/O buffer. Set once by [`Self::register_buffers`];
    /// every read/write must address bytes inside this range.
    registered: Cell<Option<RegisteredBuf>>,
    /// Map from in-flight `user_data` to its CQE result. We never
    /// share a ring with another future so this stays single-task;
    /// it exists only to handle the case where `submit_and_wait(1)`
    /// returns a completion for a *different* in-flight op than the
    /// one we are waiting on. With the current single-op-at-a-time
    /// call sites it stays empty, but the bookkeeping is cheap and
    /// keeps the loop correct if a caller ever drives multiple
    /// operations concurrently on the same task.
    pending: RefCell<HashMap<u64, i32>>,
    next_user_data: Cell<u64>,
    page_size: usize,
    capacity_pages: u64,
    queue_depth: u32,
    _no_send: PhantomData<*const ()>,
}

#[derive(Copy, Clone)]
struct RegisteredBuf {
    base: NonNull<u8>,
    len: usize,
}

impl UringBlockDevice {
    /// Open `path` and build a ring configured per `cfg`.
    ///
    /// The file is opened `O_RDWR` plus `O_DIRECT` when `cfg.o_direct`
    /// is set. Capacity is derived from `metadata().len() /
    /// page_size`; raw block devices return their full size from
    /// `metadata()` on Linux, so this works for both regular files
    /// and `/dev/nvmeXnY`.
    pub fn open(path: &Path, cfg: UringConfig) -> Result<Self, Error> {
        if cfg.page_size == 0 {
            return Err(Error::Io(libc::EINVAL));
        }
        let file = open_file(path, cfg.o_direct)?;
        let capacity_pages = file_capacity_pages(&file, cfg.page_size)?;

        let mut builder = IoUring::builder();
        if cfg.iopoll {
            builder.setup_iopoll();
        }
        if cfg.single_issuer {
            builder.setup_single_issuer();
        }
        if cfg.defer_taskrun {
            builder.setup_defer_taskrun();
        }
        let ring = builder
            .build(cfg.queue_depth)
            .map_err(io_err_to_storage)?;

        ring.submitter()
            .register_files(&[file.as_raw_fd()])
            .map_err(io_err_to_storage)?;

        Ok(Self {
            file,
            ring: RefCell::new(ring),
            registered: Cell::new(None),
            pending: RefCell::new(HashMap::new()),
            next_user_data: Cell::new(1),
            page_size: cfg.page_size,
            capacity_pages,
            queue_depth: cfg.queue_depth,
            _no_send: PhantomData,
        })
    }

    fn alloc_user_data(&self) -> u64 {
        let v = self.next_user_data.get();
        // Avoid u64::MAX so it can be reserved by callers if needed.
        let next = v.checked_add(1).unwrap_or(1);
        self.next_user_data.set(next);
        v
    }

    fn buffer_offset(&self, ptr: *const u8, len: usize) -> Result<u32, Error> {
        let reg = self.registered.get().ok_or(Error::Io(libc::EINVAL))?;
        let base = reg.base.as_ptr() as usize;
        let start = ptr as usize;
        if start < base {
            return Err(Error::Io(libc::EFAULT));
        }
        let end = start
            .checked_add(len)
            .ok_or(Error::Io(libc::EOVERFLOW))?;
        if end > base + reg.len {
            return Err(Error::Io(libc::EFAULT));
        }
        let off = start - base;
        u32::try_from(off).map_err(|_| Error::Io(libc::EOVERFLOW))
    }

    fn submit_and_wait_for(&self, want: u64) -> Result<i32, Error> {
        loop {
            if let Some(res) = self.pending.borrow_mut().remove(&want) {
                return Ok(res);
            }
            let mut ring = self.ring.borrow_mut();
            ring.submit_and_wait(1).map_err(io_err_to_storage)?;
            // Drain everything currently available so that the next
            // submit_and_wait does not block on already-reaped CQEs.
            let mut got_target = false;
            let mut target_result = 0i32;
            let mut cq = ring.completion();
            cq.sync();
            while let Some(cqe) = cq.next() {
                let ud = cqe.user_data();
                let res = cqe.result();
                if ud == want {
                    got_target = true;
                    target_result = res;
                } else {
                    self.pending.borrow_mut().insert(ud, res);
                }
            }
            if got_target {
                return Ok(target_result);
            }
        }
    }
}

impl BlockDevice for UringBlockDevice {
    fn page_size(&self) -> usize {
        self.page_size
    }

    fn capacity_pages(&self) -> u64 {
        self.capacity_pages
    }

    fn register_buffers(&self, base: *mut u8, len: usize) -> Result<(), Error> {
        let nn = NonNull::new(base).ok_or(Error::Io(libc::EINVAL))?;
        let iov = libc::iovec {
            iov_base: base as *mut libc::c_void,
            iov_len: len,
        };
        // SAFETY: caller owns `base..base+len` for the lifetime of
        // the device per the BlockDevice contract. The kernel will
        // hold the pages registered until the ring is dropped or
        // `unregister_buffers` is called.
        unsafe {
            self.ring
                .borrow()
                .submitter()
                .register_buffers(&[iov])
                .map_err(io_err_to_storage)?;
        }
        self.registered.set(Some(RegisteredBuf { base: nn, len }));
        Ok(())
    }

    async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), Error> {
        if dst.len() != self.page_size {
            return Err(Error::Io(libc::EINVAL));
        }
        if lba.0 >= self.capacity_pages {
            return Err(Error::OutOfRange);
        }
        // Bounds-check the destination against the registered
        // region; READ_FIXED takes the raw pointer, not an offset,
        // so the only purpose here is to fail loudly before the
        // kernel does.
        self.buffer_offset(dst.as_ptr(), dst.len())?;
        let user_data = self.alloc_user_data();
        let offset = lba.0.saturating_mul(self.page_size as u64);
        let sqe = opcode::ReadFixed::new(
            types::Fixed(0),
            dst.as_mut_ptr(),
            dst.len() as u32,
            0, /* buf_index */
        )
        .offset(offset)
        .build()
        .user_data(user_data);
        // SAFETY: SQE references `dst` and the registered buffer;
        // we keep `dst` alive across submit_and_wait. The ring is
        // !Send so no other thread can drop the slot under us.
        unsafe {
            let mut ring = self.ring.borrow_mut();
            let mut sq = ring.submission();
            sq.push(&sqe).map_err(|_| Error::Io(libc::ENOMEM))?;
        }
        let res = self.submit_and_wait_for(user_data)?;
        if res < 0 {
            return Err(Error::Io(-res));
        }
        if res as usize != dst.len() {
            return Err(Error::Io(libc::EIO));
        }
        Ok(())
    }

    async fn write(&self, lba: Lba, src: &[u8]) -> Result<(), Error> {
        if src.len() != self.page_size {
            return Err(Error::Io(libc::EINVAL));
        }
        if lba.0 >= self.capacity_pages {
            return Err(Error::OutOfRange);
        }
        // Bounds-check the source against the registered region;
        // see [`Self::read`] for why we ignore the offset.
        self.buffer_offset(src.as_ptr(), src.len())?;
        let user_data = self.alloc_user_data();
        let offset = lba.0.saturating_mul(self.page_size as u64);
        let sqe = opcode::WriteFixed::new(
            types::Fixed(0),
            src.as_ptr(),
            src.len() as u32,
            0, /* buf_index */
        )
        .offset(offset)
        .build()
        .user_data(user_data);
        // SAFETY: see [`Self::read`].
        unsafe {
            let mut ring = self.ring.borrow_mut();
            let mut sq = ring.submission();
            sq.push(&sqe).map_err(|_| Error::Io(libc::ENOMEM))?;
        }
        let res = self.submit_and_wait_for(user_data)?;
        if res < 0 {
            return Err(Error::Io(-res));
        }
        if res as usize != src.len() {
            return Err(Error::Io(libc::EIO));
        }
        Ok(())
    }

    fn write_queue_depth(&self) -> u32 {
        self.queue_depth
    }
}

impl Drop for UringBlockDevice {
    fn drop(&mut self) {
        // Best-effort cleanup. The ring's own Drop will close the
        // ring fd; we unregister so that the kernel releases the
        // pinned buffer/file table immediately rather than waiting
        // for fd teardown.
        let sub = self.ring.get_mut().submitter();
        let _ = sub.unregister_buffers();
        let _ = sub.unregister_files();
        let _ = &self.file; // keep the file alive until drop runs.
    }
}

fn open_file(path: &Path, o_direct: bool) -> Result<File, Error> {
    let cpath = CString::new(path.as_os_str().as_encoded_bytes())
        .map_err(|_| Error::Io(libc::EINVAL))?;
    let mut flags = libc::O_RDWR | libc::O_CLOEXEC;
    if o_direct {
        flags |= libc::O_DIRECT;
    }
    // SAFETY: cpath is null-terminated and outlives the call.
    let fd = unsafe { libc::open(cpath.as_ptr(), flags) };
    if fd < 0 {
        return Err(Error::Io(io::Error::last_os_error()
            .raw_os_error()
            .unwrap_or(libc::EIO)));
    }
    // SAFETY: fd is freshly opened by us and not aliased elsewhere.
    Ok(unsafe { File::from_raw_fd(fd as RawFd) })
}

fn file_capacity_pages(file: &File, page_size: usize) -> Result<u64, Error> {
    let meta = file.metadata().map_err(io_err_to_storage)?;
    let mut len = meta.len();
    // For raw block devices `metadata().len()` is 0 on Linux; fall
    // back to the BLKGETSIZE64 ioctl.
    if len == 0 {
        len = blkgetsize64(file.as_raw_fd())?;
    }
    Ok(len / page_size as u64)
}

fn blkgetsize64(fd: RawFd) -> Result<u64, Error> {
    // _IOR('o', 0x40 + 18, size_of::<u64>()) - we hardcode the value
    // rather than computing it from the `nix` crate to avoid pulling
    // in another dependency for one constant.
    const BLKGETSIZE64: libc::c_ulong = 0x80081272;
    let mut out: u64 = 0;
    // SAFETY: out is a writable u64 of the correct size.
    let rc = unsafe { libc::ioctl(fd, BLKGETSIZE64, &mut out as *mut u64) };
    if rc != 0 {
        return Err(Error::Io(io::Error::last_os_error()
            .raw_os_error()
            .unwrap_or(libc::EIO)));
    }
    Ok(out)
}

fn io_err_to_storage(e: io::Error) -> Error {
    Error::Io(e.raw_os_error().unwrap_or(libc::EIO))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::future::Future;
    use std::io::Write as _;
    use std::path::PathBuf;
    use std::pin::Pin;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::task::{Context, Poll, Wake, Waker};

    struct NoopWake;
    impl Wake for NoopWake {
        fn wake(self: Arc<Self>) {}
    }

    fn block_on<F: Future>(fut: F) -> F::Output {
        let waker: Waker = Arc::new(NoopWake).into();
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

    fn aligned_buffer(len: usize) -> (Vec<u8>, *mut u8) {
        // Page-aligned via Box<[u64]> backing.
        let words = (len + 7) / 8;
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
        let dev = UringBlockDevice::open(&path.0, UringConfig::test_local())
            .expect("open uring device");
        assert_eq!(dev.page_size(), page_size);
        assert_eq!(dev.capacity_pages(), pages);
        assert_eq!(dev.write_queue_depth(), 8);
    }

    #[test]
    fn read_back_what_we_wrote() {
        let (page_size, pages) = geometry();
        let path = TempPath(make_tempfile(pages, page_size));
        let dev = UringBlockDevice::open(&path.0, UringConfig::test_local())
            .expect("open uring device");

        let buf_len = page_size * 4;
        let (_owner, base) = aligned_buffer(buf_len);
        dev.register_buffers(base, buf_len)
            .expect("register buffers");

        // SAFETY: we own the buffer for the duration of the test.
        let src = unsafe { std::slice::from_raw_parts_mut(base, page_size) };
        for (i, b) in src.iter_mut().enumerate() {
            *b = (i as u8).wrapping_mul(31).wrapping_add(7);
        }
        block_on(dev.write(Lba(3), src)).expect("write");

        let dst_ptr = unsafe { base.add(page_size * 2) };
        let dst = unsafe { std::slice::from_raw_parts_mut(dst_ptr, page_size) };
        dst.fill(0);
        block_on(dev.read(Lba(3), dst)).expect("read");

        for (i, b) in dst.iter().enumerate() {
            assert_eq!(*b, (i as u8).wrapping_mul(31).wrapping_add(7), "byte {i}");
        }
    }

    #[test]
    fn read_unwritten_page_returns_seed_bytes() {
        let (page_size, pages) = geometry();
        let path = TempPath(make_tempfile(pages, page_size));
        let dev = UringBlockDevice::open(&path.0, UringConfig::test_local())
            .expect("open uring device");

        let buf_len = page_size;
        let (_owner, base) = aligned_buffer(buf_len);
        dev.register_buffers(base, buf_len).unwrap();

        let dst = unsafe { std::slice::from_raw_parts_mut(base, page_size) };
        dst.fill(0);
        block_on(dev.read(Lba(0), dst)).expect("read");
        // make_tempfile seeds the file with 0xa5.
        assert!(dst.iter().all(|b| *b == 0xa5));
    }

    #[test]
    fn out_of_range_lba_rejected_without_io() {
        let (page_size, pages) = geometry();
        let path = TempPath(make_tempfile(pages, page_size));
        let dev = UringBlockDevice::open(&path.0, UringConfig::test_local())
            .expect("open uring device");

        let buf_len = page_size;
        let (_owner, base) = aligned_buffer(buf_len);
        dev.register_buffers(base, buf_len).unwrap();
        let dst = unsafe { std::slice::from_raw_parts_mut(base, page_size) };
        let err = block_on(dev.read(Lba(pages), dst));
        assert!(matches!(err, Err(Error::OutOfRange)));
    }

    #[test]
    fn write_without_registered_buffer_errors() {
        let (page_size, pages) = geometry();
        let path = TempPath(make_tempfile(pages, page_size));
        let dev = UringBlockDevice::open(&path.0, UringConfig::test_local())
            .expect("open uring device");

        let (_owner, base) = aligned_buffer(page_size);
        let src = unsafe { std::slice::from_raw_parts(base, page_size) };
        let err = block_on(dev.write(Lba(0), src));
        assert!(matches!(err, Err(Error::Io(_))));
    }
}
