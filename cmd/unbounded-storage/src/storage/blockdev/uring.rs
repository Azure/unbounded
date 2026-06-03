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
//! sparse table of `IORING_REGISTER_BUFFERS` slots. Each call to
//! [`UringBlockDevice::register_buffers`] fills the next free slot;
//! per-I/O the device resolves the right `buf_index` by locating
//! the destination pointer inside one of the registered regions.
//! A single registered file (`IORING_REGISTER_FILES`, `Fixed(0)`)
//! is also installed at open time.
//!
//! ## Concurrency model
//!
//! The device is fully asynchronous: each in-flight op owns an
//! [`IoSlot`] holding its eventual CQE result and the [`Waker`] of
//! the task awaiting it. The executor is expected to call
//! [`UringBlockDevice::progress`] periodically (and implicitly via
//! the [`BlockDevice::progress`] trait method) to push queued SQEs
//! to the kernel and reap any pending CQEs, waking the matching
//! task and freeing a back-pressure slot.
//!
//! Submission depth is capped at `queue_depth`; callers attempting
//! to issue beyond that point park on a `submit_waiters` queue and
//! resume in FIFO order as completions free slots.
//!
//! Tests and any non-NVMe usage use [`UringConfig::test_local`]
//! which disables `IOPOLL`, `SINGLE_ISSUER`, `DEFER_TASKRUN`, and
//! `O_DIRECT` so the same code path runs on a regular tmpfile.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::ffi::CString;
use std::fs::File;
use std::future::Future;
use std::io;
use std::marker::PhantomData;
use std::os::fd::{AsRawFd, FromRawFd, RawFd};
use std::path::Path;
use std::pin::Pin;
use std::ptr::NonNull;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

use io_uring::{IoUring, opcode, types};

use super::{BlockDevice, CoreLocalDevice};
use crate::ring::{StorageRing, StorageRingConfig};
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

/// One ring + one file descriptor + a sparse table of registered
/// buffers. Pinned to the thread that opened it;
/// `PhantomData<*const ()>` makes that non-negotiable to the type
/// system.
pub struct UringBlockDevice {
    file: File,
    ring: RefCell<IoUring>,
    /// Pre-registered I/O buffer slots. Each call to
    /// [`Self::register_buffers`] fills the next entry; reads and
    /// writes locate their slot by pointer.
    registered: RefCell<Vec<RegisteredBuf>>,
    /// Number of slots the sparse table was created with.
    /// Registrations beyond this fail with `-ENOSPC`.
    slot_capacity: u32,
    /// Per-in-flight-op state, keyed by `user_data`. Entries are
    /// inserted at submit time and removed in [`Self::progress`]
    /// once the matching CQE is reaped.
    slots: RefCell<HashMap<u64, Rc<IoSlot>>>,
    /// Number of SQEs currently submitted (or pending submission)
    /// against the kernel. Bounded by `queue_depth` via the
    /// `submit_waiters` back-pressure queue.
    submitted: Cell<u32>,
    /// High-water mark of `submitted` since the device was opened,
    /// recorded at increment time in [`Self::submit_fixed_io`]. Lets
    /// callers (and tests) observe that ops actually went in-flight
    /// even when each op completes within its own poll - the
    /// transient peak is invisible to an external sample of
    /// `submitted`, which can return to zero before it is read.
    peak_submitted: Cell<u32>,
    /// FIFO of wakers parked because `submitted == queue_depth`
    /// at submit time. Drained one-per-completion in `progress`.
    submit_waiters: RefCell<Vec<Waker>>,
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

/// Maximum number of distinct registered regions a single ring
/// can hold. Sized for "bufferpool backing + a small handful of
/// scratch / extra backings" with comfortable headroom; bump if a
/// real deployment ever needs more than this.
const MAX_REGISTERED_SLOTS: u32 = 32;

/// Per-op completion slot. The waker is registered by the
/// [`IoFut`] poll path; the result is filled by
/// [`UringBlockDevice::progress`] when the matching CQE is reaped.
struct IoSlot {
    result: Cell<Option<i32>>,
    waker: RefCell<Option<Waker>>,
}

impl IoSlot {
    fn new() -> Self {
        Self {
            result: Cell::new(None),
            waker: RefCell::new(None),
        }
    }
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
        if cfg.page_size == 0 || cfg.queue_depth == 0 {
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
        let ring = builder.build(cfg.queue_depth).map_err(io_err_to_storage)?;

        ring.submitter()
            .register_files(&[file.as_raw_fd()])
            .map_err(io_err_to_storage)?;

        Ok(Self {
            file,
            ring: RefCell::new(ring),
            registered: RefCell::new(Vec::new()),
            slot_capacity: MAX_REGISTERED_SLOTS,
            slots: RefCell::new(HashMap::new()),
            submitted: Cell::new(0),
            peak_submitted: Cell::new(0),
            submit_waiters: RefCell::new(Vec::new()),
            next_user_data: Cell::new(1),
            page_size: cfg.page_size,
            capacity_pages,
            queue_depth: cfg.queue_depth,
            _no_send: PhantomData,
        })
    }

    /// Push queued SQEs to the kernel and reap any available CQEs,
    /// waking the awaiting futures and freeing back-pressure slots.
    ///
    /// Always non-blocking: never calls `submit_and_wait(N > 0)`.
    /// The executor is responsible for calling this periodically
    /// (typically from its idle hook) so in-flight ops can complete.
    pub fn progress(&self) -> Result<(), Error> {
        // 1. Push any newly-queued SQEs to the kernel without
        // waiting. With IOPOLL, this also gives the kernel a chance
        // to drive polling.
        {
            let ring = self.ring.borrow_mut();
            ring.submitter().submit().map_err(io_err_to_storage)?;
        }

        // 2. Drain the completion queue. Collect wakers under the
        // ring borrow, then drop the borrow before waking so wake
        // handlers are free to re-enter the device.
        let mut to_wake: Vec<Waker> = Vec::new();
        {
            let mut ring = self.ring.borrow_mut();
            let mut cq = ring.completion();
            cq.sync();
            while let Some(cqe) = cq.next() {
                let ud = cqe.user_data();
                let res = cqe.result();
                if let Some(slot) = self.slots.borrow_mut().remove(&ud) {
                    slot.result.set(Some(res));
                    if let Some(w) = slot.waker.borrow_mut().take() {
                        to_wake.push(w);
                    }
                }
                // submitted count tracks live slots, not CQEs in
                // the abstract: decrement once per reaped op.
                let n = self.submitted.get();
                debug_assert!(n > 0, "completion without outstanding submission");
                self.submitted.set(n.saturating_sub(1));

                // Free one back-pressure waiter per completion.
                if let Some(w) = pop_front_waker(&mut self.submit_waiters.borrow_mut()) {
                    to_wake.push(w);
                }
            }
        }

        for w in to_wake {
            w.wake();
        }
        Ok(())
    }

    fn alloc_user_data(&self) -> u64 {
        let v = self.next_user_data.get();
        // Avoid u64::MAX so it can be reserved by callers if needed.
        let next = v.checked_add(1).unwrap_or(1);
        self.next_user_data.set(next);
        v
    }

    fn resolve_buf_index(&self, ptr: *const u8, len: usize) -> Result<u16, Error> {
        let regs = self.registered.borrow();
        if regs.is_empty() {
            return Err(Error::Io(libc::EINVAL));
        }
        let start = ptr as usize;
        let end = start.checked_add(len).ok_or(Error::Io(libc::EOVERFLOW))?;
        for (idx, reg) in regs.iter().enumerate() {
            let base = reg.base.as_ptr() as usize;
            if start >= base && end <= base + reg.len {
                return Ok(idx as u16);
            }
        }
        Err(Error::Io(libc::EFAULT))
    }

    /// Wait (cooperatively) for a free submission slot, then push
    /// `sqe` and resolve once the kernel completes it.
    async fn submit_fixed_io(
        &self,
        sqe: io_uring::squeue::Entry,
        expected_len: usize,
    ) -> Result<(), Error> {
        SubmitSlot { dev: self }.await;

        let user_data = sqe.get_user_data();
        let slot = Rc::new(IoSlot::new());
        self.slots.borrow_mut().insert(user_data, Rc::clone(&slot));
        self.submitted.set(self.submitted.get() + 1);
        if self.submitted.get() > self.peak_submitted.get() {
            self.peak_submitted.set(self.submitted.get());
        }

        // SAFETY: SQE references caller-owned buffers within the
        // registered region; the caller keeps them alive until this
        // future resolves. The ring is !Send so no other thread can
        // drop the slot under us.
        let push_res = unsafe {
            let mut ring = self.ring.borrow_mut();
            let mut sq = ring.submission();
            sq.push(&sqe)
        };
        if push_res.is_err() {
            // Roll back bookkeeping if the SQ was unexpectedly full.
            self.slots.borrow_mut().remove(&user_data);
            self.submitted.set(self.submitted.get().saturating_sub(1));
            // Some waiter may now fit; wake one.
            if let Some(w) = pop_front_waker(&mut self.submit_waiters.borrow_mut()) {
                w.wake();
            }
            return Err(Error::Io(libc::ENOMEM));
        }

        let res = IoFut {
            dev: self,
            slot,
            done: false,
        }
        .await?;

        if res < 0 {
            return Err(Error::Io(-res));
        }
        if res as usize != expected_len {
            return Err(Error::Io(libc::EIO));
        }
        Ok(())
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
        // Compute `was_nonempty` up front so we do not hold a borrow
        // of `self.registered` across the unsafe block, where the
        // mirror gets mutated.
        let was_nonempty = {
            let regs = self.registered.borrow();
            if regs.len() as u32 >= self.slot_capacity {
                return Err(Error::Io(libc::ENOSPC));
            }
            !regs.is_empty()
        };
        // The io-uring 0.6 surface only exposes the "replace the
        // whole table" form, so re-register every region every time.
        //
        // INVARIANT: `self.registered` must always mirror the
        // kernel-side table. This path is NOT open-time only:
        // `shard_view::replay_locked` re-registers buffers at runtime
        // during a hot-swap. `unregister_buffers` empties the kernel
        // table, so if the subsequent `register_buffers` fails we must
        // leave the mirror EMPTY (matching the now-empty kernel table)
        // rather than the stale prior set; otherwise `resolve_buf_index`
        // would hand out indices into a table the kernel no longer has
        // and every later READ_FIXED/WRITE_FIXED would fail with EFAULT.
        // The error is still propagated, so the caller (replay) must
        // tolerate a returned error.
        let mut new_regs = self.registered.borrow().clone();
        new_regs.push(RegisteredBuf { base: nn, len });
        let iovs: Vec<libc::iovec> = new_regs
            .iter()
            .map(|r| libc::iovec {
                iov_base: r.base.as_ptr() as *mut libc::c_void,
                iov_len: r.len,
            })
            .collect();
        // SAFETY: every (base, len) in `new_regs` was provided by a
        // caller that owns the region for the lifetime of the
        // device; we keep `new_regs` parallel to the kernel-side
        // table.
        let register_result = unsafe {
            let submitter = self.ring.borrow();
            let submitter = submitter.submitter();
            // If there is already a table, drop it first; the
            // kernel rejects a second `register_buffers` otherwise.
            if was_nonempty {
                let _ = submitter.unregister_buffers();
            }
            submitter.register_buffers(&iovs).map_err(io_err_to_storage)
        };
        let mut mirror = self.registered.borrow_mut();
        apply_registration_result(&mut mirror, new_regs, register_result)
    }

    async fn read(&self, lba: Lba, dst: &mut [u8]) -> Result<(), Error> {
        if dst.is_empty() || dst.len() % self.page_size != 0 {
            return Err(Error::Io(libc::EINVAL));
        }
        let n_pages = (dst.len() / self.page_size) as u64;
        if lba
            .0
            .checked_add(n_pages)
            .is_none_or(|end| end > self.capacity_pages)
        {
            return Err(Error::OutOfRange);
        }
        // Locate which registered region holds `dst` so we can
        // address it via READ_FIXED's buf_index. -EFAULT here
        // surfaces as a clean Err rather than a kernel rejection.
        let buf_index = self.resolve_buf_index(dst.as_ptr(), dst.len())?;
        let user_data = self.alloc_user_data();
        let offset = lba.0.saturating_mul(self.page_size as u64);
        let sqe = opcode::ReadFixed::new(
            types::Fixed(0),
            dst.as_mut_ptr(),
            dst.len() as u32,
            buf_index,
        )
        .offset(offset)
        .build()
        .user_data(user_data);
        self.submit_fixed_io(sqe, dst.len()).await
    }

    async fn write(&self, lba: Lba, src: &[u8]) -> Result<(), Error> {
        if src.is_empty() || src.len() % self.page_size != 0 {
            return Err(Error::Io(libc::EINVAL));
        }
        let n_pages = (src.len() / self.page_size) as u64;
        if lba
            .0
            .checked_add(n_pages)
            .is_none_or(|end| end > self.capacity_pages)
        {
            return Err(Error::OutOfRange);
        }
        // See [`Self::read`] for buf_index resolution.
        let buf_index = self.resolve_buf_index(src.as_ptr(), src.len())?;
        let user_data = self.alloc_user_data();
        let offset = lba.0.saturating_mul(self.page_size as u64);
        let sqe =
            opcode::WriteFixed::new(types::Fixed(0), src.as_ptr(), src.len() as u32, buf_index)
                .offset(offset)
                .build()
                .user_data(user_data);
        self.submit_fixed_io(sqe, src.len()).await
    }

    fn write_queue_depth(&self) -> u32 {
        self.queue_depth
    }

    fn progress(&self) -> Result<(), Error> {
        UringBlockDevice::progress(self)
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

/// Back-pressure park: yields `Pending` while `submitted ==
/// queue_depth`, registering the caller's waker on each poll so
/// `progress` can resume it when a slot frees up.
struct SubmitSlot<'a> {
    dev: &'a UringBlockDevice,
}

impl<'a> Future for SubmitSlot<'a> {
    type Output = ();
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let dev = self.dev;
        if dev.submitted.get() < dev.queue_depth {
            return Poll::Ready(());
        }
        // Try to opportunistically drive completions to free a slot
        // before parking.
        let _ = dev.progress();
        if dev.submitted.get() < dev.queue_depth {
            return Poll::Ready(());
        }
        dev.submit_waiters.borrow_mut().push(cx.waker().clone());
        Poll::Pending
    }
}

/// Future that resolves once the kernel returns a CQE for a slot.
struct IoFut<'a> {
    dev: &'a UringBlockDevice,
    slot: Rc<IoSlot>,
    done: bool,
}

impl<'a> Future for IoFut<'a> {
    type Output = Result<i32, Error>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Result<i32, Error>> {
        let this = self.get_mut();
        if this.done {
            return Poll::Pending;
        }
        if let Some(res) = this.slot.result.get() {
            this.done = true;
            return Poll::Ready(Ok(res));
        }
        // Opportunistically peek the CQ; if the kernel has the
        // result ready we can finish without a re-poll round-trip.
        if let Err(e) = this.dev.progress() {
            this.done = true;
            return Poll::Ready(Err(e));
        }
        if let Some(res) = this.slot.result.get() {
            this.done = true;
            return Poll::Ready(Ok(res));
        }
        *this.slot.waker.borrow_mut() = Some(cx.waker().clone());
        Poll::Pending
    }
}

/// Opens a disk for a pinned storage core.
///
/// Unlike [`UringBlockDevice`], which owns its ring and is therefore
/// `!Send`, this path produces a [`StorageRing`] the storage core owns
/// separately (so it can install the ring into the thread-local
/// registry and drive it alongside the engine) plus a `Send + Sync`
/// [`CoreLocalDevice`] that resolves that ring at call time. The whole
/// block-device lifecycle - io_uring setup flags, `O_DIRECT`, the
/// `BLKGETSIZE64` capacity probe, and file registration - lives here
/// rather than in the supervisor.
pub struct UringDevice;

impl UringDevice {
    /// Open `path` for a storage core: build the ring per `ring_cfg`,
    /// open the file (`O_DIRECT` when the ring uses `IOPOLL`), size it
    /// (via `metadata().len()`, falling back to `BLKGETSIZE64` for raw
    /// block devices), register its fd to get a `Fixed` index, and bind
    /// the geometry into a [`CoreLocalDevice`].
    ///
    /// The syscall order is exactly: ring construction, file open,
    /// capacity probe, file registration. The returned [`OpenDisk`]
    /// hands the ring back un-wrapped; the caller is responsible for
    /// installing it into the thread-local registry
    /// ([`set_current_storage_ring`](crate::ring::set_current_storage_ring))
    /// on the storage-core thread, and for keeping [`OpenDisk::file`]
    /// alive for as long as the ring's registered file table addresses
    /// it.
    pub fn open(
        path: &Path,
        ring_cfg: StorageRingConfig,
        page_size: usize,
    ) -> Result<OpenDisk, OpenError> {
        let ring = StorageRing::new(ring_cfg).map_err(OpenError::Ring)?;
        // IOPOLL requires O_DIRECT; mirror that on the file open.
        let file = open_file(path, ring_cfg.iopoll).map_err(OpenError::OpenFile)?;
        let capacity_pages = file_capacity_pages(&file, page_size).map_err(OpenError::Capacity)?;
        let file_index = ring
            .register_file(file.as_raw_fd())
            .map_err(OpenError::RegisterFile)?;
        let device =
            CoreLocalDevice::new(file_index, page_size, capacity_pages, ring_cfg.queue_depth);
        Ok(OpenDisk { device, ring, file })
    }
}

/// Product of [`UringDevice::open`]: the engine-facing device, the ring
/// it resolves I/O through, and the owned backing file.
pub struct OpenDisk {
    /// `Send + Sync` device the engine is built on. Resolves the ring
    /// from the thread-local registry at call time.
    pub device: CoreLocalDevice,
    /// Ring the storage core installs into the registry and drives via
    /// [`StorageRing::progress`].
    pub ring: StorageRing,
    /// Owned fd backing the registered `Fixed` file. The kernel holds
    /// its own reference once the fd is registered, but the caller keeps
    /// this for the storage core's lifetime so the fd is not closed out
    /// from under the ring.
    pub file: File,
}

/// Failure from [`UringDevice::open`], tagged by the phase that failed
/// so the supervisor surfaces the same diagnostic it did when this
/// logic was inline on the storage-core thread.
pub enum OpenError {
    Ring(Error),
    OpenFile(Error),
    Capacity(Error),
    RegisterFile(Error),
}

impl std::fmt::Display for OpenError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            OpenError::Ring(e) => write!(f, "storage ring: {e}"),
            OpenError::OpenFile(e) => write!(f, "open disk: {e}"),
            OpenError::Capacity(e) => write!(f, "disk capacity: {e}"),
            OpenError::RegisterFile(e) => write!(f, "register file: {e}"),
        }
    }
}

/// Pop the oldest waker from `v`, treating it as a FIFO. `Vec` is
/// fine here because the queue is bounded by `queue_depth` and we
/// only manipulate it on the owning thread.
fn pop_front_waker(v: &mut Vec<Waker>) -> Option<Waker> {
    if v.is_empty() {
        None
    } else {
        Some(v.remove(0))
    }
}

/// Reconcile the local mirror of the kernel registered-buffer table
/// after a "unregister then register" swap.
///
/// By the time this is called the kernel table has already been
/// emptied (`unregister_buffers`) and re-registration attempted. On
/// success the mirror becomes the full `new_regs` table. On failure
/// the kernel table is empty, so the mirror is cleared to match rather
/// than left describing the stale prior set; the error is returned.
fn apply_registration_result(
    mirror: &mut Vec<RegisteredBuf>,
    new_regs: Vec<RegisteredBuf>,
    register_result: Result<(), Error>,
) -> Result<(), Error> {
    match register_result {
        Ok(()) => {
            *mirror = new_regs;
            Ok(())
        }
        Err(e) => {
            mirror.clear();
            Err(e)
        }
    }
}

fn open_file(path: &Path, o_direct: bool) -> Result<File, Error> {
    let cpath =
        CString::new(path.as_os_str().as_encoded_bytes()).map_err(|_| Error::Io(libc::EINVAL))?;
    let mut flags = libc::O_RDWR | libc::O_CLOEXEC;
    if o_direct {
        flags |= libc::O_DIRECT;
    }
    // SAFETY: cpath is null-terminated and outlives the call.
    let fd = unsafe { libc::open(cpath.as_ptr(), flags) };
    if fd < 0 {
        return Err(Error::Io(
            io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO),
        ));
    }
    // SAFETY: fd is freshly opened by us and not aliased elsewhere.
    Ok(unsafe { File::from_raw_fd(fd as RawFd) })
}

/// Create `path` if absent and size it to exactly `size_bytes`.
///
/// `ftruncate` sets the file length (growing or shrinking as needed);
/// then `fallocate` (mode 0) backs the range with real blocks so a
/// later write cannot hit ENOSPC mid-run. On filesystems that do not
/// support fallocate (for example tmpfs) the allocation step is
/// skipped and the file is left sparse at the requested length.
///
/// Intended for `kind = "file"` disks; production block devices never
/// go through here.
pub fn provision_file(path: &Path, size_bytes: u64) -> Result<(), Error> {
    let cpath =
        CString::new(path.as_os_str().as_encoded_bytes()).map_err(|_| Error::Io(libc::EINVAL))?;
    // SAFETY: cpath is null-terminated and outlives the call. The mode
    // argument is consumed because O_CREAT is set.
    let fd = unsafe {
        libc::open(
            cpath.as_ptr(),
            libc::O_RDWR | libc::O_CREAT | libc::O_CLOEXEC,
            0o644 as libc::c_uint,
        )
    };
    if fd < 0 {
        return Err(Error::Io(
            io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO),
        ));
    }
    // Wrap the fd immediately so it is closed on every return path.
    // SAFETY: fd is freshly opened by us and not aliased elsewhere.
    let file = unsafe { File::from_raw_fd(fd as RawFd) };

    // SAFETY: file.as_raw_fd() is a valid, owned descriptor.
    let rc = unsafe { libc::ftruncate(file.as_raw_fd(), size_bytes as libc::off_t) };
    if rc != 0 {
        return Err(Error::Io(
            io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO),
        ));
    }

    // SAFETY: same owned descriptor; mode 0 is a plain allocation.
    let rc = unsafe { libc::fallocate(file.as_raw_fd(), 0, 0, size_bytes as libc::off_t) };
    if rc != 0 {
        let err = io::Error::last_os_error()
            .raw_os_error()
            .unwrap_or(libc::EIO);
        // tmpfs and some other backends do not implement fallocate;
        // the file keeps its truncated length and stays sparse.
        if err == libc::EOPNOTSUPP || err == libc::ENOTSUP || err == libc::ENOSYS {
            return Ok(());
        }
        return Err(Error::Io(err));
    }
    Ok(())
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
        return Err(Error::Io(
            io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO),
        ));
    }
    Ok(out)
}

fn io_err_to_storage(e: io::Error) -> Error {
    Error::Io(e.raw_os_error().unwrap_or(libc::EIO))
}

#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::io::Write as _;
    use std::path::PathBuf;
    use std::pin::Pin;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicU64, Ordering};
    use std::task::{Context, Poll, Wake, Waker};

    use super::*;

    static SEQ: AtomicU64 = AtomicU64::new(0);

    struct NoopWake;
    impl Wake for NoopWake {
        fn wake(self: Arc<Self>) {}
    }

    fn noop_waker() -> Waker {
        Arc::new(NoopWake).into()
    }

    /// Drive multiple futures cooperatively while calling
    /// `dev.progress()` between polls. Returns the collected outputs
    /// in submission order once every future is ready.
    fn pump<O>(
        dev: &UringBlockDevice,
        mut futs: Vec<Pin<Box<dyn Future<Output = O> + '_>>>,
    ) -> Vec<O> {
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
            dev.progress().expect("progress");
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

    fn make_tempfile(pages: u64, page_size: usize, fill: u8) -> PathBuf {
        let n = SEQ.fetch_add(1, Ordering::Relaxed);
        let mut p = std::env::temp_dir();
        p.push(format!("uring-async-{}-{}.bin", std::process::id(), n));
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
        p
    }

    struct TempPath(PathBuf);
    impl Drop for TempPath {
        fn drop(&mut self) {
            let _ = std::fs::remove_file(&self.0);
        }
    }

    fn aligned_buffer(len: usize) -> (Vec<u8>, *mut u8) {
        let words = (len + 7) / 8;
        let mut v = vec![0u8; words * 8 + 4096];
        let raw = v.as_mut_ptr();
        let aligned_off = raw.align_offset(4096);
        let base = unsafe { raw.add(aligned_off) };
        (v, base)
    }

    #[test]
    fn concurrent_reads_complete_correctly() {
        const PAGE: usize = 4096;
        const PAGES: u64 = 16;
        let path = TempPath(make_tempfile(PAGES, PAGE, 0xa5));
        let cfg = UringConfig {
            queue_depth: 16,
            ..UringConfig::test_local()
        };
        let dev = UringBlockDevice::open(&path.0, cfg).expect("open");

        // Register one buffer big enough for 8 distinct destination
        // slices.
        let buf_len = PAGE * 8;
        let (_owner, base) = aligned_buffer(buf_len);
        dev.register_buffers(base, buf_len).unwrap();

        // Issue 8 concurrent reads against the first 8 LBAs into
        // disjoint slices of the registered buffer.
        let mut futs: Vec<Pin<Box<dyn Future<Output = Result<(), Error>> + '_>>> =
            Vec::with_capacity(8);
        let mut dst_ptrs = Vec::with_capacity(8);
        for i in 0..8u64 {
            let ptr = unsafe { base.add((i as usize) * PAGE) };
            dst_ptrs.push(ptr);
            let slice = unsafe { std::slice::from_raw_parts_mut(ptr, PAGE) };
            slice.fill(0);
            futs.push(Box::pin(dev.read(Lba(i), slice)));
        }

        let results = pump(&dev, futs);
        for r in &results {
            r.as_ref().expect("read ok");
        }
        for ptr in dst_ptrs {
            let slice = unsafe { std::slice::from_raw_parts(ptr, PAGE) };
            assert!(slice.iter().all(|b| *b == 0xa5), "each page seeded 0xa5");
        }
    }

    #[test]
    fn submit_backpressure_parks_and_resumes() {
        const PAGE: usize = 4096;
        const PAGES: u64 = 16;
        const QD: u32 = 4;
        let path = TempPath(make_tempfile(PAGES, PAGE, 0));
        let cfg = UringConfig {
            queue_depth: QD,
            ..UringConfig::test_local()
        };
        let dev = UringBlockDevice::open(&path.0, cfg).expect("open");

        // Register a single page-sized buffer; all 16 writes share
        // it (write-only semantics on the device don't require
        // unique source bytes for this back-pressure test).
        let buf_len = PAGE;
        let (_owner, base) = aligned_buffer(buf_len);
        dev.register_buffers(base, buf_len).unwrap();
        let src = unsafe { std::slice::from_raw_parts_mut(base, PAGE) };
        for (i, b) in src.iter_mut().enumerate() {
            *b = (i as u8).wrapping_mul(7);
        }
        let src_const: &[u8] = unsafe { std::slice::from_raw_parts(base, PAGE) };

        let mut futs: Vec<Pin<Box<dyn Future<Output = Result<(), Error>> + '_>>> =
            Vec::with_capacity(16);
        for i in 0..16u64 {
            futs.push(Box::pin(dev.write(Lba(i), src_const)));
        }

        // Drive the pump and check the invariant on every step.
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
                    dev.submitted.get() <= QD,
                    "submitted={} exceeds queue_depth={}",
                    dev.submitted.get(),
                    QD,
                );
            }
            dev.progress().expect("progress");
            assert!(dev.submitted.get() <= QD);
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
        // peak high-water mark is read from the device rather than
        // sampled externally: with buffered I/O each write can complete
        // within its own poll (the opportunistic `progress` in
        // `IoFut::poll` reaps the CQE and decrements `submitted` back to
        // zero before this loop ever samples it), so an external sample
        // can legitimately never observe a non-zero in-flight count.
        let peak = dev.peak_submitted.get();
        assert!(peak > 0, "should have had in-flight ops");
        assert!(peak <= QD);
    }

    fn unique_path(name: &str) -> PathBuf {
        let n = SEQ.fetch_add(1, Ordering::Relaxed);
        let mut p = std::env::temp_dir();
        p.push(format!(
            "uring-provision-{}-{}-{}.bin",
            name,
            std::process::id(),
            n
        ));
        p
    }

    #[test]
    fn provision_file_creates_and_sizes() {
        const PAGE: usize = 4096;
        let path = TempPath(unique_path("creates"));
        assert!(!path.0.exists());

        provision_file(&path.0, (16 * PAGE) as u64).expect("provision");

        let len = std::fs::metadata(&path.0).unwrap().len();
        assert_eq!(len, (16 * PAGE) as u64);

        let file = open_file(&path.0, false).expect("open");
        let pages = file_capacity_pages(&file, PAGE).expect("capacity");
        assert_eq!(pages, 16);
    }

    #[test]
    fn provision_file_grows_then_shrinks() {
        const PAGE: usize = 4096;
        let path = TempPath(unique_path("resize"));

        provision_file(&path.0, (8 * PAGE) as u64).expect("provision 8");
        assert_eq!(std::fs::metadata(&path.0).unwrap().len(), (8 * PAGE) as u64);

        provision_file(&path.0, (4 * PAGE) as u64).expect("provision 4");
        assert_eq!(std::fs::metadata(&path.0).unwrap().len(), (4 * PAGE) as u64);

        provision_file(&path.0, (12 * PAGE) as u64).expect("provision 12");
        assert_eq!(
            std::fs::metadata(&path.0).unwrap().len(),
            (12 * PAGE) as u64
        );
    }

    fn dummy_reg(addr: usize, len: usize) -> RegisteredBuf {
        RegisteredBuf {
            base: NonNull::new(addr as *mut u8).expect("nonnull"),
            len,
        }
    }

    /// On a failed re-register, the mirror must be left EMPTY to match
    /// the now-empty kernel table, never the stale prior set. This is
    /// the core of the desync bug: a non-empty stale mirror would feed
    /// `resolve_buf_index` indices the kernel no longer has.
    #[test]
    fn apply_registration_result_clears_mirror_on_failure() {
        let mut mirror = vec![dummy_reg(0x1000, 4096)];
        let new_regs = vec![dummy_reg(0x1000, 4096), dummy_reg(0x2000, 4096)];
        let res = apply_registration_result(&mut mirror, new_regs, Err(Error::Io(libc::EFAULT)));
        assert!(matches!(res, Err(Error::Io(libc::EFAULT))));
        assert!(
            mirror.is_empty(),
            "mirror must be empty after a failed re-register, got {} entries",
            mirror.len()
        );
    }

    /// On success the mirror adopts the full new table.
    #[test]
    fn apply_registration_result_adopts_table_on_success() {
        let mut mirror = vec![dummy_reg(0x1000, 4096)];
        let new_regs = vec![dummy_reg(0x1000, 4096), dummy_reg(0x2000, 4096)];
        let res = apply_registration_result(&mut mirror, new_regs, Ok(()));
        assert!(res.is_ok());
        assert_eq!(mirror.len(), 2);
        assert_eq!(mirror[0].base.as_ptr() as usize, 0x1000);
        assert_eq!(mirror[1].base.as_ptr() as usize, 0x2000);
    }

    /// The success path through the real device keeps the mirror
    /// consistent with the kernel: both registered regions resolve to
    /// distinct buffer indices.
    #[test]
    fn register_buffers_success_keeps_mirror_consistent() {
        const PAGE: usize = 4096;
        const PAGES: u64 = 16;
        let path = TempPath(make_tempfile(PAGES, PAGE, 0));
        let dev = UringBlockDevice::open(&path.0, UringConfig::test_local()).expect("open");

        let (_owner_a, base_a) = aligned_buffer(PAGE);
        dev.register_buffers(base_a, PAGE).expect("register a");
        assert_eq!(dev.registered.borrow().len(), 1);

        let (_owner_b, base_b) = aligned_buffer(PAGE);
        dev.register_buffers(base_b, PAGE).expect("register b");
        assert_eq!(dev.registered.borrow().len(), 2);

        // Both regions remain addressable via their buffer indices,
        // proving the re-register left the mirror parallel to the
        // kernel table.
        assert_eq!(dev.resolve_buf_index(base_a, PAGE).unwrap(), 0);
        assert_eq!(dev.resolve_buf_index(base_b, PAGE).unwrap(), 1);
    }
}
