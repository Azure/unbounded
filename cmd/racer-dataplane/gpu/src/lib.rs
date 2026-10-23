//! CUDA support for Racer GPUDirect reads (`designs/racer-gpudirect.md`).
//!
//! Every CUDA, NVML and cuFile call goes through a small C shim,
//! `libracer_gpu_cuda.so.1` (`native/gpu_cuda.c`), loaded with `dlopen` and
//! checked with `racer_gpu_abi()`. All types here are `!Send`: build and use
//! them on the dataplane's native helper thread, never on I/O threads.
//!
//! # Basics
//!
//! 1. [`Gpu::load`] loads the backend and initializes CUDA.
//! 2. [`Gpu::devices`] lists GPUs; [`DeviceInfo::usable`] skips GPUs in
//!    `EXCLUSIVE_PROCESS` or `PROHIBITED` compute mode.
//! 3. [`Gpu::context`] retains a GPU's primary context.
//! 4. [`Context::import`] maps a client allocation (a POSIX fd shareable
//!    handle) for one request. Writes are limited to the request's window.
//! 5. [`Context::staging`] allocates dataplane-owned device memory for
//!    ciphertext. It can be filled from the host, from disk with cuFile
//!    ([`GpuFile::read`]) or by a NIC through a dma-buf ([`StagingBuffer::dma_buf`]).
//! 6. [`Stream`] queues copies and [`Stream::decrypt`], then
//!    [`Stream::synchronize`] waits for them. Only then is the write visible.
//!
//! # Lifetimes and leaks
//!
//! A stream keeps every buffer it touched alive until a successful
//! synchronize, so an import is never unmapped while queued GPU work may still
//! write it. If a free, unmap or synchronize fails, the resource is leaked on
//! purpose, counted in [`Gpu::leaked`], and the shim stays loaded.
//!
//! # Backends
//!
//! - `native`: loads `libracer_gpu_cuda.so.1`.
//! - `simulation`: [`simulation::Simulation`], fake GPUs in host memory.
//! - Neither: [`Gpu::load`] returns [`Error::Unavailable`].
//!
//! Hardware-free tests: `cargo test -p racer-gpu --features simulation`.
mod ffi;
pub mod gcm;

#[cfg(any(test, feature = "simulation"))]
pub mod simulation;

pub use ffi::{MAX_AAD, MAX_DECRYPT, flag};

use ffi::{Out, RawDecrypt, RawDevice, RawVerdict, Shared};
use std::{
    cell::{Cell, RefCell},
    ffi::c_void,
    ops::Range,
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    ptr::NonNull,
    rc::Rc,
};

/// Result type for this crate.
pub type Result<T> = std::result::Result<T, Error>;

/// Errors from this crate.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// No backend, no driver, or the feature is not built in.
    Unavailable,

    /// Bad arguments or handle.
    InvalidRequest,

    /// An offset or length is outside a buffer or the request window.
    InvalidRange,

    /// The allocation or buffer is on another GPU.
    WrongDevice,

    /// A CUDA, NVML or cuFile call failed with this status.
    Driver(i32),

    /// Host or device memory allocation failed.
    OutOfMemory,

    /// The shim was built without decrypt kernels, or they failed to load.
    KernelUnavailable,

    /// A cuFile read failed or was short.
    Io,

    /// Wrong state: work in flight, or no verdict to read.
    Busy,

    /// The call failed and a resource was leaked on purpose.
    Leaked,
}

impl std::fmt::Display for Error {
    /// Describe the error.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Driver(code) => write!(f, "GPU driver call failed with status {code}"),
            other => write!(f, "{other:?}"),
        }
    }
}

impl std::error::Error for Error {}

/// A GPU's compute mode (`CUcomputemode`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ComputeMode {
    /// Several contexts may share the GPU.
    Default,

    /// No contexts allowed.
    Prohibited,

    /// One process at a time.
    ExclusiveProcess,

    /// Unknown, or the query failed.
    Other(i32),
}

impl ComputeMode {
    /// From the raw `CUcomputemode` value.
    fn from_raw(value: i32) -> Self {
        match value {
            0 => Self::Default,
            2 => Self::Prohibited,
            3 => Self::ExclusiveProcess,
            other => Self::Other(other),
        }
    }
}

/// One GPU from discovery.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeviceInfo {
    /// CUDA device ordinal.
    pub ordinal: i32,

    /// GPU UUID.
    pub uuid: [u8; 16],

    /// Lowercase PCI address, for example `0000:3b:00.0`.
    pub pci_bus_id: String,

    /// NUMA node from NVML or sysfs, if known.
    pub numa_node: Option<u32>,

    /// Compute mode.
    pub compute_mode: ComputeMode,

    /// `flag::*` capability bits.
    pub flags: u32,

    /// Device memory in bytes.
    pub total_memory: u64,
}

impl DeviceInfo {
    /// Whether the dataplane can use this GPU: default compute mode, and it
    /// can import POSIX fd handles into virtual memory.
    pub fn usable(&self) -> bool {
        self.compute_mode == ComputeMode::Default
            && self.flags & (flag::VMM | flag::POSIX_FD) == flag::VMM | flag::POSIX_FD
    }

    /// The UUID in `GPU-xxxxxxxx-xxxx-...` form, as `nvidia-smi` prints it.
    pub fn uuid_string(&self) -> String {
        let hex: String = self.uuid.iter().map(|b| format!("{b:02x}")).collect();
        format!(
            "GPU-{}-{}-{}-{}-{}",
            &hex[..8],
            &hex[8..12],
            &hex[12..16],
            &hex[16..20],
            &hex[20..]
        )
    }

    /// From the C record.
    fn from_raw(raw: &RawDevice) -> Self {
        let end = raw.pci_bus_id.iter().position(|&b| b == 0).unwrap_or(32);
        Self {
            ordinal: raw.ordinal,
            uuid: raw.uuid,
            pci_bus_id: String::from_utf8_lossy(&raw.pci_bus_id[..end]).into_owned(),
            numa_node: u32::try_from(raw.numa_node).ok(),
            compute_mode: ComputeMode::from_raw(raw.compute_mode),
            flags: raw.flags,
            total_memory: raw.total_memory,
        }
    }
}

/// Call a shim function that writes a handle; return it or the error.
fn create(shared: &Shared, call: impl FnOnce(Out) -> i32) -> Result<NonNull<c_void>> {
    let mut out = std::ptr::null_mut();
    shared.check(call(&mut out))?;
    NonNull::new(out).ok_or(Error::InvalidRequest)
}

/// A loaded GPU backend with CUDA initialized.
pub struct Gpu {
    shared: Rc<Shared>,
}

impl Gpu {
    /// Load the backend and initialize CUDA. Inside a
    /// [`simulation::Simulation::enter`] scope this uses the simulation.
    pub fn load() -> Result<Self> {
        #[cfg(any(test, feature = "simulation"))]
        let api = match simulation::current() {
            Some(_) => simulation::api(),
            None => ffi::Api::load()?,
        };
        #[cfg(not(any(test, feature = "simulation")))]
        let api = ffi::Api::load()?;
        let shared = Rc::new(Shared {
            api,
            leaks: Cell::new(0),
        });
        shared.check(unsafe { (shared.api.init)() })?;
        Ok(Self { shared })
    }

    /// Every GPU, including ones that are not [`DeviceInfo::usable`].
    pub fn devices(&self) -> Result<Vec<DeviceInfo>> {
        let mut raw = vec![RawDevice::default(); 64];
        let count = unsafe { (self.shared.api.discover)(raw.as_mut_ptr(), raw.len() as u32) };
        self.shared.check(count.min(0))?;
        let count = count as usize;
        if count > raw.len() {
            return Err(Error::InvalidRequest);
        }
        Ok(raw[..count].iter().map(DeviceInfo::from_raw).collect())
    }

    /// The usable GPUs only.
    pub fn usable_devices(&self) -> Result<Vec<DeviceInfo>> {
        Ok(self
            .devices()?
            .into_iter()
            .filter(DeviceInfo::usable)
            .collect())
    }

    /// Retain a GPU's primary context.
    pub fn context(&self, ordinal: i32) -> Result<Context> {
        let shared = &self.shared;
        let raw = create(shared, |out| unsafe { (shared.api.context)(ordinal, out) })?;
        Ok(Context(Rc::new(ContextInner {
            shared: shared.clone(),
            raw,
            ordinal,
        })))
    }

    /// Open the cuFile driver if needed and register an `O_DIRECT` file.
    pub fn open_file(&self, fd: OwnedFd) -> Result<GpuFile> {
        let shared = &self.shared;
        shared.check(unsafe { (shared.api.file_open)() })?;
        let fd = cufile_descriptor(fd);
        let raw = create(shared, |out| unsafe {
            (shared.api.file)(fd.as_raw_fd(), out)
        })?;
        Ok(GpuFile {
            shared: shared.clone(),
            raw,
            fd: Some(fd),
        })
    }

    /// Resources leaked on purpose since this backend was loaded.
    pub fn leaked(&self) -> u64 {
        self.shared.leaks.get()
    }
}

/// A retained primary context. Clones share it; it is released when the last
/// clone and every object made from it are gone.
#[derive(Clone)]
pub struct Context(Rc<ContextInner>);

/// The owned context handle.
struct ContextInner {
    shared: Rc<Shared>,

    raw: NonNull<c_void>,

    ordinal: i32,
}

impl Drop for ContextInner {
    /// Release the context, or leak it if that fails.
    fn drop(&mut self) {
        if unsafe { (self.shared.api.context_free)(self.raw.as_ptr()) } != 0 {
            self.shared.leak();
        }
    }
}

impl Context {
    /// The CUDA device ordinal.
    pub fn ordinal(&self) -> i32 {
        self.0.ordinal
    }

    /// Leaks seen by this context's backend.
    pub fn leaked(&self) -> u64 {
        self.0.shared.leaks.get()
    }

    /// Create a non-blocking stream.
    pub fn stream(&self) -> Result<Stream> {
        let shared = &self.0.shared;
        let raw = create(shared, |out| unsafe {
            (shared.api.stream)(self.0.raw.as_ptr(), out)
        })?;
        Ok(Stream {
            context: self.clone(),
            raw,
            retained: RefCell::new(Vec::new()),
        })
    }

    /// Import a client allocation and map all `allocation_size` bytes
    /// read-write. Writes are then allowed only inside `window`, the
    /// request's `[offset, offset + length)`. Keeps `fd` until the import is
    /// released, then closes it.
    pub fn import(
        &self,
        fd: OwnedFd,
        allocation_size: u64,
        window: Range<u64>,
    ) -> Result<ImportedAllocation> {
        if window.start >= window.end || window.end > allocation_size {
            return Err(Error::InvalidRange);
        }
        let shared = &self.0.shared;
        let raw = create(shared, |out| unsafe {
            (shared.api.import)(self.0.raw.as_ptr(), fd.as_raw_fd(), allocation_size, out)
        })?;
        Ok(ImportedAllocation(Rc::new(ImportInner {
            context: self.clone(),
            raw,
            size: allocation_size,
            window,
            fd: Cell::new(Some(fd)),
        })))
    }

    /// Allocate `size` bytes of staging memory (rounded up to 2 MiB on the
    /// device; offsets are checked against `size`).
    pub fn staging(&self, size: u64) -> Result<StagingBuffer> {
        let shared = &self.0.shared;
        let raw = create(shared, |out| unsafe {
            (shared.api.staging)(self.0.raw.as_ptr(), size, out)
        })?;
        Ok(StagingBuffer(Rc::new(StagingInner {
            context: self.clone(),
            raw,
            size,
            registered: Cell::new(false),
            in_flight: Cell::new(0),
        })))
    }
}

/// A client allocation mapped into the dataplane's context for one request.
pub struct ImportedAllocation(Rc<ImportInner>);

/// The mapping. Freed when the handle and every stream using it let go.
struct ImportInner {
    context: Context,

    raw: NonNull<c_void>,

    size: u64,

    window: Range<u64>,

    fd: Cell<Option<OwnedFd>>,
}

impl Drop for ImportInner {
    /// Unmap and release, then close the fd. On failure leak everything,
    /// including the fd and the context.
    fn drop(&mut self) {
        let shared = &self.context.0.shared;
        if unsafe { (shared.api.import_free)(self.raw.as_ptr()) } != 0 {
            shared.leak();
            std::mem::forget(self.fd.take());
            std::mem::forget(self.context.clone());
        }
    }
}

impl ImportedAllocation {
    /// The size passed to `cuMemCreate`.
    pub fn size(&self) -> u64 {
        self.0.size
    }

    /// The writable window.
    pub fn window(&self) -> Range<u64> {
        self.0.window.clone()
    }

    /// The GPU it lives on.
    pub fn ordinal(&self) -> i32 {
        self.0.context.ordinal()
    }
}

/// Dataplane-owned device memory for ciphertext.
pub struct StagingBuffer(Rc<StagingInner>);

/// The allocation.
struct StagingInner {
    context: Context,

    raw: NonNull<c_void>,

    size: u64,

    registered: Cell<bool>,

    /// Streams holding it until their next synchronize.
    in_flight: Cell<u32>,
}

impl Drop for StagingInner {
    /// Deregister from cuFile, then free. On failure leak.
    fn drop(&mut self) {
        let shared = &self.context.0.shared;
        if self.registered.get()
            && unsafe { (shared.api.staging_unregister)(self.raw.as_ptr()) } != 0
        {
            shared.leak();
            std::mem::forget(self.context.clone());
            return;
        }
        if unsafe { (shared.api.staging_free)(self.raw.as_ptr()) } != 0 {
            shared.leak();
            std::mem::forget(self.context.clone());
        }
    }
}

impl StagingBuffer {
    /// Usable bytes.
    pub fn size(&self) -> u64 {
        self.0.size
    }

    /// Whether a stream may still be using it.
    pub fn in_flight(&self) -> bool {
        self.0.in_flight.get() > 0
    }

    /// Register with cuFile so reads go straight to it.
    pub fn register_cufile(&self) -> Result<()> {
        if self.0.registered.get() {
            return Ok(());
        }
        let shared = &self.0.context.0.shared;
        shared.check(unsafe { (shared.api.file_open)() })?;
        shared.check(unsafe { (shared.api.staging_register)(self.0.raw.as_ptr()) })?;
        self.0.registered.set(true);
        Ok(())
    }

    /// Export `[offset, offset + length)` as a dma-buf fd, for
    /// `ibv_reg_dmabuf_mr`. Both must be multiples of the host page size.
    pub fn dma_buf(&self, offset: u64, length: u64) -> Result<OwnedFd> {
        let shared = &self.0.context.0.shared;
        let mut fd = -1;
        shared
            .check(unsafe { (shared.api.dmabuf)(self.0.raw.as_ptr(), offset, length, &mut fd) })?;
        if fd < 0 {
            return Err(Error::InvalidRequest);
        }
        Ok(unsafe { OwnedFd::from_raw_fd(fd) })
    }
}

/// Where a stream writes.
#[derive(Clone, Copy)]
pub enum Destination<'a> {
    /// A client allocation, limited to its window.
    Import(&'a ImportedAllocation),

    /// Dataplane staging memory.
    Staging(&'a StagingBuffer),
}

/// Something a stream keeps alive until synchronized.
enum Retained {
    Import(#[allow(dead_code)] Rc<ImportInner>),

    Staging(Rc<StagingInner>),

    /// Host bytes a queued copy may still read.
    Host(#[allow(dead_code)] Box<dyn AsRef<[u8]>>),
}

impl Drop for Retained {
    /// Release one in-flight hold on staging memory.
    fn drop(&mut self) {
        if let Self::Staging(inner) = self {
            inner.in_flight.set(inner.in_flight.get() - 1);
        }
    }
}

/// One page to verify and decrypt on the GPU.
pub struct DecryptRequest<'a> {
    /// XAES-derived AES-256-GCM key (`racer_crypto::DerivedPageKey::key`).
    pub key: &'a [u8; 32],

    /// XAES-derived GCM nonce (`racer_crypto::DerivedPageKey::nonce`).
    pub nonce: &'a [u8; 12],

    /// The page AAD (`security::page_aad`). At most [`MAX_AAD`] bytes.
    pub aad: &'a [u8],

    /// Plaintext length. The source holds this many bytes plus the tag.
    pub length: u64,

    /// Stored CRC-64/XZ of the sealed page, if known.
    pub expected_crc: Option<u64>,
}

/// The outcome of a GPU decrypt.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Verdict {
    /// Verified and written. `crc` is the sealed page's CRC-64/XZ.
    Passed {
        /// CRC-64/XZ of the sealed page.
        crc: u64,
    },

    /// The CRC did not match. Nothing was written.
    CrcMismatch {
        /// The CRC the GPU computed.
        crc: u64,
    },

    /// The tag did not match. Nothing was written.
    TagMismatch {
        /// The CRC the GPU computed.
        crc: u64,
    },

    /// Verification did not run (a launch failed). Nothing was written.
    NotRun,
}

/// A CUDA stream. Work is visible only after [`Stream::synchronize`].
pub struct Stream {
    context: Context,

    raw: NonNull<c_void>,

    retained: RefCell<Vec<Retained>>,
}

impl Stream {
    /// The backend state.
    fn shared(&self) -> &Rc<Shared> {
        &self.context.0.shared
    }

    /// Check a destination range and return its raw handle and hold.
    fn destination(
        &self,
        dst: Destination<'_>,
        offset: u64,
        length: u64,
    ) -> Result<(*mut c_void, Retained)> {
        let end = offset.checked_add(length).ok_or(Error::InvalidRange)?;
        match dst {
            Destination::Import(import) => {
                let inner = &import.0;
                if !Rc::ptr_eq(&inner.context.0, &self.context.0) {
                    return Err(Error::WrongDevice);
                }
                if offset < inner.window.start || end > inner.window.end {
                    return Err(Error::InvalidRange);
                }
                Ok((inner.raw.as_ptr(), Retained::Import(inner.clone())))
            }
            Destination::Staging(staging) => self.source(staging, offset, length),
        }
    }

    /// Check a staging range and return its raw handle and hold.
    fn source(
        &self,
        staging: &StagingBuffer,
        offset: u64,
        length: u64,
    ) -> Result<(*mut c_void, Retained)> {
        let inner = &staging.0;
        if !Rc::ptr_eq(&inner.context.0, &self.context.0) {
            return Err(Error::WrongDevice);
        }
        let end = offset.checked_add(length).ok_or(Error::InvalidRange)?;
        if end > inner.size {
            return Err(Error::InvalidRange);
        }
        inner.in_flight.set(inner.in_flight.get() + 1);
        Ok((inner.raw.as_ptr(), Retained::Staging(inner.clone())))
    }

    /// Keep holds until the next synchronize. Held even when the call
    /// failed, because part of it may have been queued.
    fn retain(&self, holds: impl IntoIterator<Item = Retained>) {
        self.retained.borrow_mut().extend(holds);
    }

    /// Queue a copy of host bytes. CUDA may keep reading page-locked host
    /// memory after the call returns, so the stream takes ownership of
    /// `bytes` and holds it until [`Stream::synchronize`] succeeds (or leaks
    /// it if the stream cannot be synchronized). Pass a cheap shared handle,
    /// such as an `Arc`, to avoid copying large buffers.
    pub fn copy_from_host<S>(&self, dst: Destination<'_>, offset: u64, bytes: S) -> Result<()>
    where
        S: AsRef<[u8]> + 'static,
    {
        // Box first so the bytes do not move after the pointer is taken.
        let source: Box<dyn AsRef<[u8]>> = Box::new(bytes);
        let slice = (*source).as_ref();
        let (ptr, length) = (slice.as_ptr(), slice.len() as u64);
        if length == 0 {
            return Err(Error::InvalidRange);
        }
        let (raw, hold) = self.destination(dst, offset, length)?;
        let rc = unsafe {
            (self.shared().api.copy_h2d)(self.raw.as_ptr(), raw, offset, ptr.cast(), length)
        };
        self.retain([hold, Retained::Host(source)]);
        self.shared().check(rc)
    }

    /// Number of host sources held for queued copies. Test helper.
    #[doc(hidden)]
    pub fn retained_host_sources(&self) -> usize {
        self.retained
            .borrow()
            .iter()
            .filter(|hold| matches!(hold, Retained::Host(_)))
            .count()
    }

    /// Queue a device copy from staging memory.
    pub fn copy_from_staging(
        &self,
        dst: Destination<'_>,
        dst_offset: u64,
        src: &StagingBuffer,
        src_offset: u64,
        length: u64,
    ) -> Result<()> {
        if length == 0 {
            return Err(Error::InvalidRange);
        }
        let (raw_dst, dst_hold) = self.destination(dst, dst_offset, length)?;
        let (raw_src, src_hold) = self.source(src, src_offset, length)?;
        let rc = unsafe {
            (self.shared().api.copy_d2d)(
                self.raw.as_ptr(),
                raw_dst,
                dst_offset,
                raw_src,
                src_offset,
                length,
            )
        };
        self.retain([dst_hold, src_hold]);
        self.shared().check(rc)
    }

    /// Copy staging bytes to the host and wait for this stream. Client
    /// imports can never be read.
    pub fn read_staging(&self, src: &StagingBuffer, offset: u64, out: &mut [u8]) -> Result<()> {
        if out.is_empty() {
            return Err(Error::InvalidRange);
        }
        let (raw, hold) = self.source(src, offset, out.len() as u64)?;
        let rc = unsafe {
            (self.shared().api.copy_d2h)(
                self.raw.as_ptr(),
                out.as_mut_ptr().cast(),
                raw,
                offset,
                out.len() as u64,
            )
        };
        self.retain([hold]);
        self.shared().check(rc)?;
        // copy_d2h synchronized the stream.
        self.retained.borrow_mut().clear();
        Ok(())
    }

    /// Queue verify-then-decrypt of the sealed page at `src_offset` into
    /// `dst` at `dst_offset`. The destination is written only if the checks
    /// pass, decided on the GPU. Read the result with [`Stream::verdict`]
    /// after [`Stream::synchronize`]. One decrypt per synchronize; a
    /// second one first returns [`Error::Busy`].
    pub fn decrypt(
        &self,
        request: &DecryptRequest<'_>,
        src: &StagingBuffer,
        src_offset: u64,
        dst: Destination<'_>,
        dst_offset: u64,
    ) -> Result<()> {
        if request.length > MAX_DECRYPT || request.aad.len() > MAX_AAD {
            return Err(Error::InvalidRequest);
        }
        let mut holds = Vec::with_capacity(2);
        let (raw_src, hold) = self.source(src, src_offset, request.length + 16)?;
        holds.push(hold);
        // A zero-length page writes nothing but still needs a destination
        // on this GPU inside the window.
        let (raw_dst, hold) = self.destination(dst, dst_offset, request.length)?;
        holds.push(hold);
        let raw = RawDecrypt {
            key: *request.key,
            nonce: *request.nonce,
            check_crc: request.expected_crc.is_some() as u32,
            expected_crc: request.expected_crc.unwrap_or(0),
            length: request.length,
            aad: request.aad.as_ptr(),
            aad_length: request.aad.len() as u64,
        };
        let rc = unsafe {
            (self.shared().api.decrypt)(
                self.raw.as_ptr(),
                &raw,
                raw_src,
                src_offset,
                raw_dst,
                dst_offset,
            )
        };
        drop(raw);
        self.retain(holds);
        self.shared().check(rc)
    }

    /// Wait for all queued work, then release holds on the buffers it used.
    pub fn synchronize(&self) -> Result<()> {
        self.shared()
            .check(unsafe { (self.shared().api.stream_sync)(self.raw.as_ptr()) })?;
        self.retained.borrow_mut().clear();
        Ok(())
    }

    /// The verdict of the last decrypt, after [`Stream::synchronize`].
    pub fn verdict(&self) -> Result<Verdict> {
        let mut raw = RawVerdict::default();
        self.shared()
            .check(unsafe { (self.shared().api.verdict)(self.raw.as_ptr(), &mut raw) })?;
        Ok(match raw.status {
            gcm::VERDICT_PASS => Verdict::Passed { crc: raw.crc },
            gcm::VERDICT_CRC => Verdict::CrcMismatch { crc: raw.crc },
            gcm::VERDICT_TAG => Verdict::TagMismatch { crc: raw.crc },
            _ => Verdict::NotRun,
        })
    }
}

impl Drop for Stream {
    /// Synchronize and free. If that fails, leak the stream and every buffer
    /// it may still be using.
    fn drop(&mut self) {
        let shared = self.context.0.shared.clone();
        if unsafe { (shared.api.stream_free)(self.raw.as_ptr()) } != 0 {
            shared.leak();
            std::mem::forget(std::mem::take(&mut *self.retained.borrow_mut()));
            std::mem::forget(self.context.clone());
        }
    }
}

/// Return a descriptor cuFile accepts for the same open file.
///
/// cuFile refuses to register a descriptor whose status flags include
/// `O_NOFOLLOW` (`CU_FILE_INVALID_FILE_OPEN_FLAG`, 5019), and Racer opens every
/// slab file and block device with it. Reopening `/proc/self/fd/N` follows the
/// kernel's link to the already-open file, not a path, so dropping the flag
/// here does not reintroduce the symlink race it guards against. The new
/// descriptor keeps the access mode and `O_DIRECT`, and is used only if it
/// names the same file. Otherwise the original is returned unchanged and
/// cuFile reports its own error.
fn cufile_descriptor(fd: OwnedFd) -> OwnedFd {
    let raw = fd.as_raw_fd();
    let flags = unsafe { libc::fcntl(raw, libc::F_GETFL) };
    if flags < 0 || flags & libc::O_NOFOLLOW == 0 {
        return fd;
    }
    let path = format!("/proc/self/fd/{raw}\0");
    let reopen = (flags & !(libc::O_NOFOLLOW | libc::O_CREAT | libc::O_EXCL | libc::O_TRUNC))
        | libc::O_CLOEXEC;
    let new = unsafe { libc::open(path.as_ptr().cast(), reopen) };
    if new < 0 {
        return fd;
    }
    let new = unsafe { OwnedFd::from_raw_fd(new) };
    match (stat(&fd), stat(&new)) {
        (Some(a), Some(b))
            if a.st_dev == b.st_dev && a.st_ino == b.st_ino && a.st_rdev == b.st_rdev =>
        {
            new
        }
        _ => fd,
    }
}

fn stat(fd: &OwnedFd) -> Option<libc::stat> {
    let mut out = std::mem::MaybeUninit::<libc::stat>::uninit();
    if unsafe { libc::fstat(fd.as_raw_fd(), out.as_mut_ptr()) } != 0 {
        return None;
    }
    Some(unsafe { out.assume_init() })
}

/// A file registered with cuFile.
pub struct GpuFile {
    shared: Rc<Shared>,

    raw: NonNull<c_void>,

    fd: Option<OwnedFd>,
}

impl GpuFile {
    /// Read `length` bytes at `file_offset` into staging at `offset`. Waits
    /// for the read. The staging buffer must not be in use by a stream.
    pub fn read(
        &self,
        staging: &StagingBuffer,
        offset: u64,
        file_offset: u64,
        length: u64,
    ) -> Result<()> {
        if staging.in_flight() {
            return Err(Error::Busy);
        }
        let end = offset.checked_add(length).ok_or(Error::InvalidRange)?;
        if length == 0 || end > staging.size() {
            return Err(Error::InvalidRange);
        }
        let n = unsafe {
            (self.shared.api.file_read)(
                self.raw.as_ptr(),
                staging.0.raw.as_ptr(),
                offset,
                file_offset,
                length,
            )
        };
        if n < 0 {
            return self.shared.check(n as i32);
        }
        if n as u64 != length {
            return Err(Error::Io);
        }
        Ok(())
    }
}

impl Drop for GpuFile {
    /// Deregister, then close the fd. On failure leak both.
    fn drop(&mut self) {
        if unsafe { (self.shared.api.file_free)(self.raw.as_ptr()) } != 0 {
            self.shared.leak();
            std::mem::forget(self.fd.take());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn open(path: &std::path::Path, flags: i32) -> OwnedFd {
        let path = std::ffi::CString::new(path.as_os_str().as_encoded_bytes()).unwrap();
        let fd = unsafe { libc::open(path.as_ptr(), flags | libc::O_CLOEXEC) };
        assert!(fd >= 0, "open: {}", std::io::Error::last_os_error());
        unsafe { OwnedFd::from_raw_fd(fd) }
    }

    fn status(fd: &OwnedFd) -> i32 {
        unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFL) }
    }

    fn temp_file(name: &str) -> std::path::PathBuf {
        let path = std::env::temp_dir().join(format!("racer-gpu-{}-{name}", std::process::id()));
        std::fs::write(&path, b"racer").unwrap();
        path
    }

    #[test]
    /// cuFile rejects O_NOFOLLOW, so the descriptor is reopened without it
    /// and keeps the same file and access mode.
    fn cufile_descriptor_drops_nofollow() {
        let path = temp_file("nofollow");
        let fd = open(&path, libc::O_RDWR | libc::O_NOFOLLOW);
        let before = stat(&fd).unwrap();
        let fd = cufile_descriptor(fd);
        let after = stat(&fd).unwrap();
        let flags = status(&fd);
        assert_eq!(flags & libc::O_NOFOLLOW, 0);
        assert_eq!(flags & libc::O_ACCMODE, libc::O_RDWR);
        assert_eq!((before.st_dev, before.st_ino), (after.st_dev, after.st_ino));
        let cloexec = unsafe { libc::fcntl(fd.as_raw_fd(), libc::F_GETFD) };
        assert_ne!(cloexec & libc::FD_CLOEXEC, 0);
        std::fs::remove_file(path).unwrap();
    }

    #[test]
    /// An unlinked file is still reopened through the kernel's link.
    fn cufile_descriptor_follows_open_file_not_path() {
        let path = temp_file("unlinked");
        let fd = open(&path, libc::O_RDONLY | libc::O_NOFOLLOW);
        let before = stat(&fd).unwrap();
        std::fs::remove_file(&path).unwrap();
        std::fs::write(&path, b"other").unwrap();
        let fd = cufile_descriptor(fd);
        let after = stat(&fd).unwrap();
        assert_eq!(status(&fd) & libc::O_NOFOLLOW, 0);
        assert_eq!((before.st_dev, before.st_ino), (after.st_dev, after.st_ino));
        std::fs::remove_file(path).unwrap();
    }

    #[test]
    /// A descriptor without O_NOFOLLOW is returned as is.
    fn cufile_descriptor_keeps_other_descriptors() {
        let path = temp_file("plain");
        let fd = open(&path, libc::O_RDONLY);
        let raw = fd.as_raw_fd();
        let fd = cufile_descriptor(fd);
        assert_eq!(fd.as_raw_fd(), raw);
        std::fs::remove_file(path).unwrap();
    }

    #[test]
    /// Without a backend in scope or built in, loading fails cleanly.
    fn load_without_backend_is_unavailable() {
        #[cfg(not(feature = "native"))]
        assert_eq!(Gpu::load().err(), Some(Error::Unavailable));
    }

    #[test]
    /// Compute mode and capabilities gate usability.
    fn usable_requires_default_mode_and_vmm_fd_support() {
        let mut device = DeviceInfo {
            ordinal: 0,
            uuid: [0xab; 16],
            pci_bus_id: "0000:3b:00.0".into(),
            numa_node: Some(1),
            compute_mode: ComputeMode::Default,
            flags: flag::VMM | flag::POSIX_FD,
            total_memory: 1,
        };
        assert!(device.usable());
        assert_eq!(
            device.uuid_string(),
            "GPU-abababab-abab-abab-abab-abababababab"
        );
        for mode in [
            ComputeMode::Prohibited,
            ComputeMode::ExclusiveProcess,
            ComputeMode::Other(-1),
        ] {
            device.compute_mode = mode;
            assert!(!device.usable());
        }
        device.compute_mode = ComputeMode::Default;
        device.flags = flag::VMM;
        assert!(!device.usable());
    }
}
