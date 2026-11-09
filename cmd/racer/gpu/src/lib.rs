//! CUDA support for Racer GPUDirect reads (`designs/racer-gpudirect.md`).
//!
//! Every CUDA, NVML and cuFile call goes through a small C shim,
//! `libracer_gpu_cuda.so.1` (`native/gpu_cuda.c`), loaded with `dlopen` and
//! checked with `racer_gpu_abi()`. The types here are `Send` and `Sync`
//! (except [`Stream`], which is only `Send`), so
//! the dataplane can run one executor thread per GPU and still reach another
//! GPU's buffers for peer copies, but every call may block on the driver:
//! use them on GPU executor threads, never on I/O threads. [`FileReader`]
//! runs cuFile reads that [`GpuFile::detach`] prepared on other threads, so
//! slow disk reads do not hold up an executor. A thread that is not the one
//! that loaded a simulated backend calls [`Gpu::enter_thread`] first.
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
//! Tests on a real GPU: `make racer-gpu-test` (`tests/hardware.rs`).
mod ffi;
pub mod gcm;

#[cfg(any(test, feature = "simulation"))]
pub mod simulation;

pub use ffi::{MAX_AAD, MAX_DECRYPT, flag};

use ffi::rdma_scope;

use ffi::{Out, RawDecrypt, RawDevice, RawVerdict, Shared};
use std::{
    ffi::{c_int, c_void},
    ops::Range,
    os::fd::{AsRawFd, FromRawFd, OwnedFd},
    ptr::NonNull,
    sync::{
        Arc, Mutex, MutexGuard,
        atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering},
    },
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

    /// cuFile does not support this file, its filesystem, or its device,
    /// with this cuFile status. Files on other devices may still work.
    Unsupported(i32),
}

impl std::fmt::Display for Error {
    /// Describe the error.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Driver(code) => write!(f, "GPU driver call failed with status {code}"),
            Self::Unsupported(code) => {
                write!(f, "cuFile does not support the file (status {code})")
            }
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
    pub fn display_id(&self) -> String {
        display_id(&self.uuid)
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

/// A loaded GPU backend with CUDA initialized. Cheap to clone.
#[derive(Clone)]
pub struct Gpu {
    shared: Arc<Shared>,

    /// The simulation the backend was loaded in.
    #[cfg(any(test, feature = "simulation"))]
    simulation: Option<simulation::Simulation>,
}

/// Returned by [`Gpu::enter_thread`].
pub struct ThreadGuard {
    #[cfg(any(test, feature = "simulation"))]
    _environment: Option<simulation::Environment>,
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
        let shared = Arc::new(Shared {
            api,
            leaks: AtomicU64::new(0),
            file_flags: AtomicU32::new(0),
        });
        shared.check(unsafe { (shared.api.init)() })?;
        Ok(Self {
            shared,
            #[cfg(any(test, feature = "simulation"))]
            simulation: simulation::current(),
        })
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
        Ok(Context(Arc::new(ContextInner {
            shared: shared.clone(),
            raw,
            ordinal,
        })))
    }

    /// Open the cuFile driver if needed and register an `O_DIRECT` file.
    pub fn open_file(&self, fd: OwnedFd) -> Result<GpuFile> {
        let shared = &self.shared;
        shared
            .check(unsafe { (shared.api.file_open)(shared.file_flags.load(Ordering::Relaxed)) })?;
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

    /// Make this backend usable on the calling thread. A no-op for the
    /// native backend; a simulated backend is entered for as long as the
    /// guard lives. Executor threads call this once when they start.
    pub fn enter_thread(&self) -> ThreadGuard {
        ThreadGuard {
            #[cfg(any(test, feature = "simulation"))]
            _environment: match (&self.simulation, simulation::current()) {
                (Some(simulation), None) => Some(simulation.enter()),
                _ => None,
            },
        }
    }

    /// Resources leaked on purpose since this backend was loaded.
    pub fn leaked(&self) -> u64 {
        self.shared.leaks.load(Ordering::Relaxed)
    }

    /// Ask cuFile to use PCI P2PDMA (`use_pci_p2pdma`) instead of
    /// nvidia-fs. Takes effect only if set before the cuFile driver opens,
    /// and applies to [`FileReader`]s made after this call. When on, a
    /// cuFile that cannot set it fails to open.
    pub fn set_pci_p2pdma(&self, enabled: bool) {
        let flags = self.shared.file_flags.load(Ordering::Relaxed) & !ffi::file_flag::PCI_P2PDMA;
        let bit = if enabled {
            ffi::file_flag::PCI_P2PDMA
        } else {
            0
        };
        self.shared.file_flags.store(flags | bit, Ordering::Relaxed);
    }

    /// A handle that runs [`FileRead`]s on any thread. It does not keep the
    /// backend loaded: drop every reader thread's work before this [`Gpu`]
    /// and the objects the reads name.
    pub fn file_reader(&self) -> FileReader {
        FileReader {
            file_open: self.shared.api.file_open,
            file_flags: self.shared.file_flags.load(Ordering::Relaxed),
            file_read: self.shared.api.file_read,
            last_driver_error: self.shared.api.last_driver_error,
            #[cfg(any(test, feature = "simulation"))]
            simulation: simulation::current(),
        }
    }
}

/// Runs cuFile reads off the helper thread. Cheap to clone.
#[derive(Clone)]
pub struct FileReader {
    file_open: unsafe extern "C" fn(u32) -> c_int,

    file_flags: u32,

    file_read: unsafe extern "C" fn(*mut c_void, *mut c_void, u64, u64, u64) -> i64,

    last_driver_error: unsafe extern "C" fn() -> i32,

    /// The simulation the backend was loaded in, entered on reader threads.
    #[cfg(any(test, feature = "simulation"))]
    simulation: Option<simulation::Simulation>,
}

impl FileReader {
    /// Open the cuFile driver, as [`Gpu::open_file`] does on first use.
    /// Opening probes every GPU and can take most of a second, so callers
    /// may do it ahead of time on another thread. A failed open is final.
    pub fn open_driver(&self) -> Result<()> {
        #[cfg(any(test, feature = "simulation"))]
        let _environment = self.enter();
        ffi::check(
            unsafe { (self.file_open)(self.file_flags) },
            self.last_driver_error,
        )
    }

    /// Run `read` and wait for it. Errors are those of [`GpuFile::read`].
    pub fn read(&self, read: FileRead) -> Result<()> {
        #[cfg(any(test, feature = "simulation"))]
        let _environment = self.enter();
        let n = unsafe {
            (self.file_read)(
                read.file.as_ptr(),
                read.staging.as_ptr(),
                read.offset,
                read.file_offset,
                read.length,
            )
        };
        if n < 0 {
            return ffi::check(n as i32, self.last_driver_error);
        }
        if n as u64 != read.length {
            return Err(Error::Io);
        }
        Ok(())
    }

    /// Enter the backend's simulation if this thread is not in one.
    #[cfg(any(test, feature = "simulation"))]
    fn enter(&self) -> Option<simulation::Environment> {
        match (&self.simulation, simulation::current()) {
            (Some(simulation), None) => Some(simulation.enter()),
            _ => None,
        }
    }
}

/// A cuFile read made by [`GpuFile::detach`], to run with [`FileReader::read`].
#[derive(Debug)]
pub struct FileRead {
    file: NonNull<c_void>,

    staging: NonNull<c_void>,

    offset: u64,

    file_offset: u64,

    length: u64,
}

// SAFETY: the shim's cuFile read is thread safe. `GpuFile::detach` makes the
// caller keep the file and staging buffer alive until the read is done.
unsafe impl Send for FileRead {}

// SAFETY for the `unsafe impl`s below: each type owns a shim handle and only
// passes it to shim calls, which are thread safe. Interior state is atomic
// or behind a mutex.
unsafe impl Send for ContextInner {}
unsafe impl Sync for ContextInner {}
unsafe impl Send for ImportInner {}
unsafe impl Sync for ImportInner {}
unsafe impl Send for StagingInner {}
unsafe impl Sync for StagingInner {}
// A stream is not `Sync`: the shim drives one stream from one thread at a
// time, and its fence bookkeeping assumes no other thread queues work on it.
unsafe impl Send for Stream {}
unsafe impl Send for GpuFile {}
unsafe impl Sync for GpuFile {}
unsafe impl Send for PinnedInner {}
unsafe impl Sync for PinnedInner {}
unsafe impl Send for Fence {}
unsafe impl Sync for Fence {}
unsafe impl Send for HostRegistration {}
unsafe impl Sync for HostRegistration {}

/// Who must see GPUDirect RDMA writes before they read the memory.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RdmaScope {
    /// Work on the GPU that owns the memory (decrypt in place).
    Owner,
    /// Every GPU (a peer copy reads the memory from another GPU).
    AllDevices,
}

/// A retained primary context. Clones share it; it is released when the last
/// clone and every object made from it are gone.
#[derive(Clone)]
pub struct Context(Arc<ContextInner>);

/// The owned context handle.
struct ContextInner {
    shared: Arc<Shared>,

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
        self.0.shared.leaks.load(Ordering::Relaxed)
    }

    /// Enable peer access so streams on this context can copy from `src`'s
    /// staging memory with [`Stream::copy_from_peer`]. Returns `false` when
    /// the two GPUs have no peer path (no NVLink or PCIe P2P), which is not
    /// an error. Returns `true` for the same GPU. Idempotent.
    pub fn enable_peer(&self, src: &Context) -> Result<bool> {
        if !Arc::ptr_eq(&self.0.shared, &src.0.shared) {
            return Err(Error::InvalidRequest);
        }
        let shared = &self.0.shared;
        let mut access: c_int = 0;
        shared.check(unsafe {
            (shared.api.peer)(self.0.raw.as_ptr(), src.0.raw.as_ptr(), &mut access)
        })?;
        Ok(access != 0)
    }

    /// Make GPUDirect RDMA writes into this GPU's memory visible to readers
    /// in `scope`. Call after a NIC wrote landed bytes and before queueing
    /// work that reads them. Free when the GPU orders such writes natively
    /// for `scope` (`CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_WRITES_ORDERING`);
    /// otherwise calls `cuFlushGPUDirectRDMAWrites`, which blocks briefly.
    /// [`Error::Unsupported`] when neither is available: the landed bytes
    /// must not be read.
    pub fn flush_rdma_writes(&self, scope: RdmaScope) -> Result<()> {
        let scope = match scope {
            RdmaScope::Owner => rdma_scope::OWNER,
            RdmaScope::AllDevices => rdma_scope::ALL,
        };
        let shared = &self.0.shared;
        shared.check(unsafe { (shared.api.flush_rdma)(self.0.raw.as_ptr(), scope) })
    }

    /// Allocate `size` bytes of page-locked host memory, usable by every
    /// context, for asynchronous host copies.
    pub fn pinned(&self, size: u64) -> Result<PinnedBuffer> {
        let shared = &self.0.shared;
        let raw = create(shared, |out| unsafe {
            (shared.api.host_alloc)(self.0.raw.as_ptr(), size, out)
        })?;
        Ok(PinnedBuffer(Arc::new(PinnedInner {
            context: self.clone(),
            raw: raw.cast(),
            size: usize::try_from(size).map_err(|_| Error::InvalidRequest)?,
            in_flight: AtomicU32::new(0),
        })))
    }

    /// Page-lock `length` bytes at `pointer` (`cuMemHostRegister`, portable),
    /// so copies from them are asynchronous DMA instead of a staged copy.
    /// Meant for long-lived pool slabs; registration is slow.
    ///
    /// # Safety
    ///
    /// The memory must stay mapped until the registration drops.
    pub unsafe fn register_host(
        &self,
        pointer: *mut u8,
        length: usize,
    ) -> Result<HostRegistration> {
        let shared = &self.0.shared;
        shared.check(unsafe {
            (shared.api.host_register)(self.0.raw.as_ptr(), pointer.cast(), length as u64)
        })?;
        Ok(HostRegistration {
            context: self.clone(),
            pointer: NonNull::new(pointer).ok_or(Error::InvalidRequest)?,
        })
    }

    /// A pool of reusable events for [`Stream::fence_in`] on this context,
    /// keeping at most `limit` idle.
    pub fn event_pool(&self, limit: usize) -> EventPool {
        EventPool(Arc::new(EventPoolInner {
            context: self.clone(),
            idle: Mutex::new(Vec::new()),
            limit,
            created: AtomicU64::new(0),
        }))
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
            retained: Mutex::new(Vec::new()),
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
        Ok(ImportedAllocation(Arc::new(ImportInner {
            context: self.clone(),
            raw,
            size: allocation_size,
            window,
            fd: Mutex::new(Some(fd)),
        })))
    }

    /// Allocate `size` bytes of staging memory (rounded up to 2 MiB on the
    /// device; offsets are checked against `size`).
    pub fn staging(&self, size: u64) -> Result<StagingBuffer> {
        let shared = &self.0.shared;
        let raw = create(shared, |out| unsafe {
            (shared.api.staging)(self.0.raw.as_ptr(), size, out)
        })?;
        Ok(StagingBuffer(Arc::new(StagingInner {
            context: self.clone(),
            raw,
            size,
            registered: AtomicBool::new(false),
            in_flight: AtomicU32::new(0),
        })))
    }
}

/// A client allocation mapped into the dataplane's context for one request.
pub struct ImportedAllocation(Arc<ImportInner>);

/// The mapping. Freed when the handle and every stream using it let go.
struct ImportInner {
    context: Context,

    raw: NonNull<c_void>,

    size: u64,

    window: Range<u64>,

    fd: Mutex<Option<OwnedFd>>,
}

impl Drop for ImportInner {
    /// Unmap and release, then close the fd. On failure leak everything,
    /// including the fd and the context.
    fn drop(&mut self) {
        let shared = &self.context.0.shared;
        if unsafe { (shared.api.import_free)(self.raw.as_ptr()) } != 0 {
            shared.leak();
            std::mem::forget(self.fd.get_mut().ok().and_then(Option::take));
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
pub struct StagingBuffer(Arc<StagingInner>);

/// The allocation.
struct StagingInner {
    context: Context,

    raw: NonNull<c_void>,

    size: u64,

    registered: AtomicBool,

    /// Streams holding it until their next synchronize.
    in_flight: AtomicU32,
}

impl Drop for StagingInner {
    /// Deregister from cuFile, then free. On failure leak.
    fn drop(&mut self) {
        let shared = &self.context.0.shared;
        if self.registered.load(Ordering::Relaxed)
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
        self.0.in_flight.load(Ordering::Acquire) > 0
    }

    /// Register with cuFile so reads go straight to it.
    pub fn register_cufile(&self) -> Result<()> {
        if self.0.registered.load(Ordering::Relaxed) {
            return Ok(());
        }
        let shared = &self.0.context.0.shared;
        shared
            .check(unsafe { (shared.api.file_open)(shared.file_flags.load(Ordering::Relaxed)) })?;
        shared.check(unsafe { (shared.api.staging_register)(self.0.raw.as_ptr()) })?;
        self.0.registered.store(true, Ordering::Relaxed);
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

/// Page-locked host memory from [`Context::pinned`]. Clones share it.
#[derive(Clone)]
pub struct PinnedBuffer(Arc<PinnedInner>);

struct PinnedInner {
    context: Context,

    raw: NonNull<u8>,

    size: usize,

    /// Streams or fences holding it until their work completes.
    in_flight: AtomicU32,
}

impl Drop for PinnedInner {
    /// Free, or leak on failure.
    fn drop(&mut self) {
        let shared = &self.context.0.shared;
        if unsafe { (shared.api.host_free)(self.context.0.raw.as_ptr(), self.raw.as_ptr().cast()) }
            != 0
        {
            shared.leak();
            std::mem::forget(self.context.clone());
        }
    }
}

impl PinnedBuffer {
    /// Bytes.
    pub fn len(&self) -> usize {
        self.0.size
    }

    /// Never true.
    pub fn is_empty(&self) -> bool {
        self.0.size == 0
    }

    /// Whether queued GPU work may still read or write it.
    pub fn in_flight(&self) -> bool {
        self.0.in_flight.load(Ordering::Acquire) > 0
    }

    /// Check `[offset, offset + length)` is inside and nothing uses it.
    fn idle(&self, offset: usize, length: usize) -> Result<()> {
        if self.in_flight() {
            return Err(Error::Busy);
        }
        match offset.checked_add(length) {
            Some(end) if end <= self.0.size => Ok(()),
            _ => Err(Error::InvalidRange),
        }
    }

    /// Copy `bytes` in at `offset`. [`Error::Busy`] while GPU work uses it.
    pub fn write(&self, offset: usize, bytes: &[u8]) -> Result<()> {
        self.idle(offset, bytes.len())?;
        // SAFETY: in range, and no GPU work or other writer uses it (the
        // in-flight check; callers hand one buffer to one thread at a time).
        unsafe {
            std::ptr::copy_nonoverlapping(
                bytes.as_ptr(),
                self.0.raw.as_ptr().add(offset),
                bytes.len(),
            )
        };
        Ok(())
    }

    /// Copy out `out.len()` bytes at `offset`. [`Error::Busy`] while GPU work
    /// uses it.
    pub fn read(&self, offset: usize, out: &mut [u8]) -> Result<()> {
        self.idle(offset, out.len())?;
        // SAFETY: as for write.
        unsafe {
            std::ptr::copy_nonoverlapping(
                self.0.raw.as_ptr().add(offset),
                out.as_mut_ptr(),
                out.len(),
            )
        };
        Ok(())
    }
}

/// Host memory page-locked by [`Context::register_host`]. Unregistered on
/// drop; leaked (left registered) if that fails.
pub struct HostRegistration {
    context: Context,

    pointer: NonNull<u8>,
}

impl Drop for HostRegistration {
    fn drop(&mut self) {
        let shared = &self.context.0.shared;
        if unsafe {
            (shared.api.host_unregister)(self.context.0.raw.as_ptr(), self.pointer.as_ptr().cast())
        } != 0
        {
            shared.leak();
        }
    }
}

/// A point in a stream's queued work, from [`Stream::fence`]. It owns the
/// holds that work needs and releases them once the work completes, so an
/// executor can keep many streams in flight and learn of completion by
/// polling instead of a blocking stream sync.
pub struct Fence {
    context: Context,

    raw: NonNull<c_void>,

    holds: Mutex<Vec<Retained>>,

    /// Where the event goes back once the fence drops, or `None` to free it.
    pool: Option<EventPool>,
}

/// CUDA events of one context kept for reuse by [`Stream::fence_in`], so a
/// busy executor does not create and destroy an event per fence. Cheap to
/// clone. Keeps at most `limit` idle events; more are freed when returned.
#[derive(Clone)]
pub struct EventPool(Arc<EventPoolInner>);

struct EventPoolInner {
    context: Context,

    idle: Mutex<Vec<NonNull<c_void>>>,

    limit: usize,

    /// Events created so far, for tests and telemetry.
    created: AtomicU64,
}

// SAFETY: the pool only stores idle event handles and passes them to shim
// calls, which are thread safe; the list is behind a mutex.
unsafe impl Send for EventPoolInner {}
unsafe impl Sync for EventPoolInner {}

impl EventPool {
    /// Idle events held now.
    pub fn idle(&self) -> usize {
        self.0
            .idle
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .len()
    }

    /// Events this pool has created so far.
    pub fn created(&self) -> u64 {
        self.0.created.load(Ordering::Relaxed)
    }

    /// The GPU the events belong to.
    pub fn ordinal(&self) -> i32 {
        self.0.context.ordinal()
    }

    /// An idle event, or a new one.
    fn take(&self) -> Result<NonNull<c_void>> {
        let idle = self
            .0
            .idle
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .pop();
        if let Some(raw) = idle {
            return Ok(raw);
        }
        let shared = &self.0.context.0.shared;
        let raw = create(shared, |out| unsafe {
            (shared.api.event)(self.0.context.0.raw.as_ptr(), out)
        })?;
        self.0.created.fetch_add(1, Ordering::Relaxed);
        Ok(raw)
    }

    /// Take back an event whose recorded work has completed.
    fn give(&self, raw: NonNull<c_void>) {
        let mut idle = self
            .0
            .idle
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        if idle.len() < self.0.limit {
            idle.push(raw);
            return;
        }
        drop(idle);
        let shared = &self.0.context.0.shared;
        if unsafe { (shared.api.event_free)(raw.as_ptr()) } != 0 {
            shared.leak();
        }
    }
}

impl Drop for EventPoolInner {
    /// Free the idle events, or leak one whose free fails.
    fn drop(&mut self) {
        let shared = &self.context.0.shared;
        let idle = std::mem::take(
            self.idle
                .get_mut()
                .unwrap_or_else(|poisoned| poisoned.into_inner()),
        );
        for raw in idle {
            if unsafe { (shared.api.event_free)(raw.as_ptr()) } != 0 {
                shared.leak();
            }
        }
    }
}

impl Fence {
    /// Whether the captured work has completed. Releases the holds when it
    /// has. Never blocks.
    pub fn poll(&self) -> Result<bool> {
        let shared = &self.context.0.shared;
        let rc = unsafe { (shared.api.event_query)(self.raw.as_ptr()) };
        if rc == ffi::code::EBUSY {
            return Ok(false);
        }
        shared.check(rc)?;
        self.release();
        Ok(true)
    }

    /// Wait for the captured work, then release the holds.
    pub fn wait(&self) -> Result<()> {
        let shared = &self.context.0.shared;
        shared.check(unsafe { (shared.api.event_sync)(self.raw.as_ptr()) })?;
        self.release();
        Ok(())
    }

    /// The GPU of the stream it marks.
    pub fn ordinal(&self) -> i32 {
        self.context.ordinal()
    }

    fn release(&self) {
        self.holds
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .clear();
    }
}

impl Drop for Fence {
    /// Wait for the work (event free synchronizes), then release. If that
    /// fails, leak the holds: the GPU may still use them.
    fn drop(&mut self) {
        let shared = self.context.0.shared.clone();
        if let Some(pool) = self.pool.take() {
            // Pooled: wait, then hand the event back for the next fence.
            if unsafe { (shared.api.event_sync)(self.raw.as_ptr()) } == 0 {
                self.release();
                pool.give(self.raw);
                return;
            }
            shared.leak();
            let holds = std::mem::take(
                &mut *self
                    .holds
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner()),
            );
            std::mem::forget(holds);
            std::mem::forget(self.context.clone());
            return;
        }
        if unsafe { (shared.api.event_free)(self.raw.as_ptr()) } != 0 {
            shared.leak();
            let holds = std::mem::take(
                &mut *self
                    .holds
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner()),
            );
            std::mem::forget(holds);
            std::mem::forget(self.context.clone());
        }
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
    // Never read: held only to keep the import alive until the stream syncs.
    Import(#[allow(dead_code)] Arc<ImportInner>),

    Staging(Arc<StagingInner>),

    /// Host bytes a queued copy may still read. Never read here, only kept alive.
    Host(#[allow(dead_code)] Box<dyn AsRef<[u8]> + Send>),

    Pinned(Arc<PinnedInner>),
}

impl Drop for Retained {
    /// Release one in-flight hold on staging memory.
    fn drop(&mut self) {
        match self {
            Self::Staging(inner) => {
                inner.in_flight.fetch_sub(1, Ordering::AcqRel);
            }
            Self::Pinned(inner) => {
                inner.in_flight.fetch_sub(1, Ordering::AcqRel);
            }
            _ => {}
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
}

/// The outcome of a GPU decrypt.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Verdict {
    /// The GCM tag matched and the plaintext was written.
    Passed,

    /// The tag did not match. Nothing was written.
    TagMismatch,

    /// Verification did not run (a launch failed). Nothing was written.
    NotRun,
}

/// A GPU UUID in `GPU-xxxxxxxx-xxxx-...` form, as `nvidia-smi` prints it.
pub fn display_id(uuid: &[u8; 16]) -> String {
    let hex: String = uuid.iter().map(|b| format!("{b:02x}")).collect();
    format!(
        "GPU-{}-{}-{}-{}-{}",
        &hex[..8],
        &hex[8..12],
        &hex[12..16],
        &hex[16..20],
        &hex[20..]
    )
}

/// A boxed [`Stream::on_complete`] callback; the host function's user data
/// points at one.
type HostCallback = Box<dyn FnOnce() + Send + 'static>;

/// The host function [`Stream::on_complete`] launches. Reclaims and runs the
/// callback exactly once. A panic is caught: it must not unwind into CUDA.
///
/// # Safety
///
/// `data` must come from `Box::into_raw(Box::new(callback))` for a
/// [`HostCallback`], and must not be used again.
unsafe extern "C" fn host_callback(data: *mut c_void) {
    if data.is_null() {
        return;
    }
    let callback = unsafe { Box::from_raw(data.cast::<HostCallback>()) };
    let _ = std::panic::catch_unwind(std::panic::AssertUnwindSafe(move || (*callback)()));
}

/// A CUDA stream. Work is visible only after [`Stream::synchronize`].
///
/// A stream is `Send` but not `Sync`: one thread drives it at a time.
///
/// ```compile_fail
/// fn sync<T: Sync>() {}
/// sync::<racer_gpu::Stream>();
/// ```
///
/// ```
/// fn send<T: Send>() {}
/// send::<racer_gpu::Stream>();
/// ```
pub struct Stream {
    context: Context,

    raw: NonNull<c_void>,

    retained: Mutex<Vec<Retained>>,
}

impl Stream {
    /// Holds on buffers and host bytes queued work may still use.
    fn held(&self) -> MutexGuard<'_, Vec<Retained>> {
        self.retained
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// The backend state.
    fn shared(&self) -> &Arc<Shared> {
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
                if !Arc::ptr_eq(&inner.context.0, &self.context.0) {
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
        if !Arc::ptr_eq(&inner.context.0, &self.context.0) {
            return Err(Error::WrongDevice);
        }
        let end = offset.checked_add(length).ok_or(Error::InvalidRange)?;
        if end > inner.size {
            return Err(Error::InvalidRange);
        }
        inner.in_flight.fetch_add(1, Ordering::AcqRel);
        Ok((inner.raw.as_ptr(), Retained::Staging(inner.clone())))
    }

    /// Keep holds until the next synchronize. Held even when the call
    /// failed, because part of it may have been queued.
    fn retain(&self, holds: impl IntoIterator<Item = Retained>) {
        self.held().extend(holds);
    }

    /// Queue a copy of host bytes. CUDA may keep reading page-locked host
    /// memory after the call returns, so the stream takes ownership of
    /// `bytes` and holds it until [`Stream::synchronize`] succeeds (or leaks
    /// it if the stream cannot be synchronized). Pass a cheap shared handle,
    /// such as an `Arc`, to avoid copying large buffers.
    pub fn copy_from_host<S>(&self, dst: Destination<'_>, offset: u64, bytes: S) -> Result<()>
    where
        S: AsRef<[u8]> + Send + 'static,
    {
        // Box first so the bytes do not move after the pointer is taken.
        let source: Box<dyn AsRef<[u8]> + Send> = Box::new(bytes);
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
        self.held()
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

    /// Queue a device copy from staging memory on another GPU, over NVLink or
    /// PCIe P2P. `dst` is on this stream's context, as for
    /// [`Stream::copy_from_staging`]; `src` is on a context this one has
    /// enabled with [`Context::enable_peer`]. Fails with
    /// [`Error::WrongDevice`] if `src` is on this stream's context (use
    /// `copy_from_staging`) or peer access is not enabled. Both buffers are
    /// held until [`Stream::synchronize`].
    pub fn copy_from_peer(
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
        let inner = &src.0;
        if Arc::ptr_eq(&inner.context.0, &self.context.0)
            || !Arc::ptr_eq(&inner.context.0.shared, self.shared())
        {
            return Err(Error::WrongDevice);
        }
        let end = src_offset.checked_add(length).ok_or(Error::InvalidRange)?;
        if end > inner.size {
            return Err(Error::InvalidRange);
        }
        let (raw_dst, dst_hold) = self.destination(dst, dst_offset, length)?;
        inner.in_flight.fetch_add(1, Ordering::AcqRel);
        let src_hold = Retained::Staging(inner.clone());
        let rc = unsafe {
            (self.shared().api.copy_peer)(
                self.raw.as_ptr(),
                raw_dst,
                dst_offset,
                inner.raw.as_ptr(),
                src_offset,
                length,
            )
        };
        self.retain([dst_hold, src_hold]);
        self.shared().check(rc)
    }

    /// Copy staging bytes to the host and wait for this stream. Client
    /// imports can never be read.
    ///
    /// The copy lands in a host buffer the stream owns, and `out` is filled
    /// only after the copy completes. If the call fails, the copy may still
    /// be queued, so the stream keeps that buffer until a later successful
    /// [`Stream::synchronize`] or leaks it if the stream is dropped first.
    pub fn read_staging(&self, src: &StagingBuffer, offset: u64, out: &mut [u8]) -> Result<()> {
        if out.is_empty() {
            return Err(Error::InvalidRange);
        }
        let (raw, hold) = self.source(src, offset, out.len() as u64)?;
        let mut landing = vec![0u8; out.len()].into_boxed_slice();
        let ptr = landing.as_mut_ptr();
        let rc = unsafe {
            (self.shared().api.copy_d2h)(
                self.raw.as_ptr(),
                ptr.cast(),
                raw,
                offset,
                out.len() as u64,
            )
        };
        self.retain([hold]);
        if rc != 0 {
            // The copy may still be queued and write the buffer later.
            self.retain([Retained::Host(Box::new(landing))]);
            return self.shared().check(rc);
        }
        // copy_d2h synchronized the stream, so the landing buffer is final.
        out.copy_from_slice(&landing);
        self.held().clear();
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
            reserved: 0,
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

    /// Check a pinned range and return its pointer and hold.
    fn pinned(
        &self,
        buffer: &PinnedBuffer,
        offset: u64,
        length: u64,
    ) -> Result<(*mut u8, Retained)> {
        let inner = &buffer.0;
        if !Arc::ptr_eq(&inner.context.0.shared, self.shared()) || length == 0 {
            return Err(Error::InvalidRequest);
        }
        let end = offset.checked_add(length).ok_or(Error::InvalidRange)?;
        if end > inner.size as u64 {
            return Err(Error::InvalidRange);
        }
        inner.in_flight.fetch_add(1, Ordering::AcqRel);
        // SAFETY: offset is inside the allocation.
        let pointer = unsafe { inner.raw.as_ptr().add(offset as usize) };
        Ok((pointer, Retained::Pinned(inner.clone())))
    }

    /// Queue a DMA copy from page-locked host memory. The buffer is held
    /// (and [`PinnedBuffer::write`] refuses it) until the work completes.
    pub fn copy_from_pinned(
        &self,
        dst: Destination<'_>,
        dst_offset: u64,
        src: &PinnedBuffer,
        src_offset: u64,
        length: u64,
    ) -> Result<()> {
        let (pointer, src_hold) = self.pinned(src, src_offset, length)?;
        let (raw, dst_hold) = match self.destination(dst, dst_offset, length) {
            Ok(found) => found,
            Err(error) => {
                drop(src_hold);
                return Err(error);
            }
        };
        let rc = unsafe {
            (self.shared().api.copy_h2d)(self.raw.as_ptr(), raw, dst_offset, pointer.cast(), length)
        };
        self.retain([dst_hold, src_hold]);
        self.shared().check(rc)
    }

    /// Queue a copy of staging bytes into page-locked host memory without
    /// waiting. Read them with [`PinnedBuffer::read`] once a fence after
    /// this completes.
    pub fn copy_to_pinned(
        &self,
        src: &StagingBuffer,
        src_offset: u64,
        dst: &PinnedBuffer,
        dst_offset: u64,
        length: u64,
    ) -> Result<()> {
        let (pointer, dst_hold) = self.pinned(dst, dst_offset, length)?;
        let (raw, src_hold) = match self.source(src, src_offset, length) {
            Ok(found) => found,
            Err(error) => {
                drop(dst_hold);
                return Err(error);
            }
        };
        let rc = unsafe {
            (self.shared().api.copy_d2h_async)(
                self.raw.as_ptr(),
                pointer.cast(),
                raw,
                src_offset,
                length,
            )
        };
        self.retain([dst_hold, src_hold]);
        self.shared().check(rc)
    }

    /// Mark everything queued so far. The holds move to the returned fence,
    /// which releases them once the work completes. If recording fails the
    /// holds stay with the stream until a later synchronize.
    pub fn fence(&self) -> Result<Fence> {
        let shared = self.shared();
        let raw = create(shared, |out| unsafe {
            (shared.api.event)(self.context.0.raw.as_ptr(), out)
        })?;
        self.record(raw, None)
    }

    /// [`Stream::fence`] with an event from `pool`, which gets it back when
    /// the fence drops. `pool` must belong to this stream's context.
    pub fn fence_in(&self, pool: &EventPool) -> Result<Fence> {
        if !Arc::ptr_eq(&pool.0.context.0, &self.context.0) {
            return Err(Error::WrongDevice);
        }
        let raw = pool.take()?;
        self.record(raw, Some(pool.clone()))
    }

    /// Record `raw` on this stream and wrap it in a fence.
    fn record(&self, raw: NonNull<c_void>, pool: Option<EventPool>) -> Result<Fence> {
        let shared = self.shared();
        let rc = unsafe { (shared.api.event_record)(raw.as_ptr(), self.raw.as_ptr()) };
        let holds = if rc == 0 {
            std::mem::take(&mut *self.held())
        } else {
            Vec::new()
        };
        let fence = Fence {
            context: self.context.clone(),
            raw,
            holds: Mutex::new(holds),
            pool,
        };
        shared.check(rc)?;
        Ok(fence)
    }

    /// Make work queued on this stream after this call wait for `fence`'s
    /// work, which may be on another GPU (`cuStreamWaitEvent`). Does not
    /// block the calling thread.
    pub fn wait_for(&self, fence: &Fence) -> Result<()> {
        if !Arc::ptr_eq(&fence.context.0.shared, self.shared()) {
            return Err(Error::InvalidRequest);
        }
        self.shared().check(unsafe {
            (self.shared().api.stream_wait)(self.raw.as_ptr(), fence.raw.as_ptr())
        })
    }

    /// Run `callback` on a CUDA-owned thread once all work queued so far on
    /// this stream completes (`cuLaunchHostFunc`). Use it to wake a thread
    /// that waits on a [`Fence`] instead of polling.
    ///
    /// `callback` must not call CUDA or anything in this crate, and should
    /// return quickly: it runs on CUDA's completion thread. It owns what it
    /// captures, so nothing it uses can be freed before it runs.
    ///
    /// [`Error::Unavailable`] when the backend or driver lacks host
    /// functions; on any error `callback` is dropped without running. If the
    /// stream's context fails, CUDA may never call it, and it is leaked.
    pub fn on_complete(&self, callback: Box<dyn FnOnce() + Send + 'static>) -> Result<()> {
        let shared = self.shared();
        let data = Box::into_raw(Box::new(callback)).cast::<c_void>();
        let rc = unsafe { (shared.api.launch_host)(self.raw.as_ptr(), host_callback, data) };
        if rc != 0 {
            // SAFETY: on error the shim never calls `host_callback`, so
            // `data` is still ours, made by `Box::into_raw` above.
            drop(unsafe { Box::from_raw(data.cast::<HostCallback>()) });
        }
        shared.check(rc)
    }

    /// The GPU this stream runs on.
    pub fn ordinal(&self) -> i32 {
        self.context.ordinal()
    }

    /// Wait for all queued work, then release holds on the buffers it used.
    pub fn synchronize(&self) -> Result<()> {
        self.shared()
            .check(unsafe { (self.shared().api.stream_sync)(self.raw.as_ptr()) })?;
        self.held().clear();
        Ok(())
    }

    /// The verdict of the last decrypt, once a synchronize or a fence after
    /// it has completed. [`Error::Busy`] before that.
    pub fn verdict(&self) -> Result<Verdict> {
        let mut raw = RawVerdict::default();
        self.shared()
            .check(unsafe { (self.shared().api.verdict)(self.raw.as_ptr(), &mut raw) })?;
        Ok(match raw.status {
            gcm::VERDICT_PASS => Verdict::Passed,
            gcm::VERDICT_TAG => Verdict::TagMismatch,
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
            std::mem::forget(std::mem::take(&mut *self.held()));
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
    shared: Arc<Shared>,

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

    /// Prepare a read of `length` bytes at `file_offset` into staging at
    /// `offset`, to run on another thread with [`FileReader::read`]. Unlike
    /// [`GpuFile::read`], streams may use other parts of the staging buffer
    /// meanwhile.
    ///
    /// # Safety
    ///
    /// Until the read returns, the caller must keep this file, `staging` and
    /// the [`Gpu`] alive, and must not let anything else touch
    /// `[offset, offset + length)` of `staging`.
    pub unsafe fn detach(
        &self,
        staging: &StagingBuffer,
        offset: u64,
        file_offset: u64,
        length: u64,
    ) -> Result<FileRead> {
        let end = offset.checked_add(length).ok_or(Error::InvalidRange)?;
        if length == 0 || end > staging.size() {
            return Err(Error::InvalidRange);
        }
        Ok(FileRead {
            file: self.raw,
            staging: staging.0.raw,
            offset,
            file_offset,
            length,
        })
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

    /// Box `callback` the way [`Stream::on_complete`] hands it to CUDA.
    fn host_data(callback: impl FnOnce() + Send + 'static) -> *mut c_void {
        let callback: HostCallback = Box::new(callback);
        Box::into_raw(Box::new(callback)).cast()
    }

    #[test]
    fn host_callback_runs_once_and_frees_its_capture() {
        let hits = Arc::new(AtomicU64::new(0));
        let held = hits.clone();
        let data = host_data(move || {
            held.fetch_add(1, Ordering::SeqCst);
        });
        assert_eq!(Arc::strong_count(&hits), 2);
        unsafe { host_callback(data) };
        assert_eq!(hits.load(Ordering::SeqCst), 1);
        assert_eq!(
            Arc::strong_count(&hits),
            1,
            "capture dropped after the call"
        );
    }

    #[test]
    fn host_callback_catches_panics_and_ignores_null() {
        let token = Arc::new(());
        let held = token.clone();
        let data = host_data(move || {
            let _held = held;
            panic!("callback panic must not unwind into CUDA");
        });
        unsafe { host_callback(data) };
        assert_eq!(Arc::strong_count(&token), 1);
        unsafe { host_callback(std::ptr::null_mut()) };
    }

    #[test]
    fn on_complete_without_host_functions_drops_the_callback() {
        let sim = simulation::Simulation::new(vec![simulation::SimDevice::new(0)]);
        let _env = sim.enter();
        let gpu = Gpu::load().unwrap();
        let stream = gpu.context(0).unwrap().stream().unwrap();
        let token = Arc::new(());
        let held = token.clone();
        let result = stream.on_complete(Box::new(move || drop(held)));
        assert_eq!(result, Err(Error::Unavailable));
        assert_eq!(Arc::strong_count(&token), 1);
    }

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
            device.display_id(),
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
