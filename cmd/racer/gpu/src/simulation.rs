//! Fake GPUs for tests. No CUDA, driver or hardware needed.
//!
//! It implements the same C functions as `native/gpu_cuda.c`, so the real
//! wrappers in `lib.rs` run on top of it unchanged. Device memory is host
//! memory; a client allocation is a memfd made with
//! [`Simulation::allocate`] and passed as the shareable handle. Decryption
//! runs the serial host emulation of the kernels (`native/gcm_host.c`).
//!
//! Work runs when queued, not on synchronize, so tests check ordering with
//! [`Simulation::trace`] instead of timing.
//!
//! # Basics
//!
//! 1. Create a [`Simulation`] with [`SimDevice`]s and [`Simulation::enter`] it.
//! 2. [`crate::Gpu::load`] now uses it.
//! 3. Script one-shot failures with [`Simulation::fault`].
//! 4. Check live resources with [`Simulation::live`].
use crate::{
    ffi::{self, Api, RawDecrypt, RawDevice, RawVerdict, code},
    gcm,
};

use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet, VecDeque},
    ffi::{c_int, c_void},
    fs::File,
    os::{
        fd::{FromRawFd, OwnedFd},
        unix::fs::{FileExt, MetadataExt},
    },
    rc::Rc,
    sync::{Arc, Mutex, MutexGuard},
};

/// Granularity imports must be a multiple of. Real GPUs use 2 MiB; smaller
/// here keeps tests light.
pub const GRANULARITY: u64 = 64 << 10;

thread_local! {
    static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) };
}

/// One simulated GPU.
#[derive(Clone, Debug)]
pub struct SimDevice {
    /// GPU UUID.
    pub uuid: [u8; 16],

    /// Lowercase PCI address.
    pub pci_bus_id: String,

    /// NUMA node, if known.
    pub numa_node: Option<u32>,

    /// Raw `CUcomputemode`.
    pub compute_mode: i32,

    /// `flag::*` bits.
    pub flags: u32,

    /// Device memory.
    pub total_memory: u64,
}

impl SimDevice {
    /// A usable GPU with every capability.
    pub fn new(index: u8) -> Self {
        Self {
            uuid: [index; 16],
            pci_bus_id: format!("0000:{:02x}:00.0", 0x10 + index),
            numa_node: Some(index as u32 % 2),
            compute_mode: 0,
            flags: ffi::flag::VMM | ffi::flag::POSIX_FD | ffi::flag::DMA_BUF | ffi::flag::GDR,
            total_memory: 80 << 30,
        }
    }
}

/// A shim function. Used in traces and to target faults.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Operation {
    Init,
    Discover,
    Context,
    ContextFree,
    Stream,
    StreamSync,
    StreamFree,
    Import,
    ImportFree,
    Staging,
    StagingFree,
    CopyH2d,
    CopyD2d,
    CopyD2h,
    Peer,
    CopyPeer,
    Decrypt,
    Verdict,
    FileOpen,
    File,
    FileFree,
    StagingRegister,
    StagingUnregister,
    FileRead,
    Dmabuf,
}

/// One call in the trace.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Event {
    /// The function.
    pub operation: Operation,

    /// The handle it acted on or created, 0 if none.
    pub handle: usize,

    /// Its result code.
    pub result: i64,
}

/// Count of live resources of each kind.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Live {
    pub contexts: usize,
    pub streams: usize,
    pub imports: usize,
    pub staging: usize,
    pub files: usize,
}

/// A simulated set of GPUs, shared by clones. Deterministic.
#[derive(Clone)]
pub struct Simulation {
    world: Arc<Mutex<World>>,
}

/// Scope guard from [`Simulation::enter`]. Restores the previous one on drop.
pub struct Environment {
    previous: Option<Simulation>,
}

impl Drop for Environment {
    /// Put back the previous simulation.
    fn drop(&mut self) {
        CURRENT.with(|current| *current.borrow_mut() = self.previous.take());
    }
}

/// A retained context.
struct SimContext {
    ordinal: i32,
}

/// A stream and its last verdict.
struct SimStream {
    context: usize,

    /// 0 none, 1 queued, 2 synchronized, as in the shim.
    decrypt: u8,

    verdict: RawVerdict,
}

/// Device memory: client memfd or staging bytes.
enum Memory {
    Import { file: File },
    Staging { bytes: Mapped, registered: bool },
}

/// Staging bytes in a shared memfd mapping. A dma-buf export of the whole
/// buffer is a duplicate of the memfd, so a simulated NIC that maps it writes
/// bytes the simulated GPU then reads, as with real GPUDirect RDMA.
struct Mapped {
    file: File,
    pointer: std::ptr::NonNull<u8>,
    length: usize,
}

// SAFETY: the mapping is owned and only reached through the world's lock.
unsafe impl Send for Mapped {}

impl Mapped {
    /// A zeroed shared mapping of `length` bytes, or None.
    fn new(length: usize) -> Option<Self> {
        let fd = unsafe { libc::memfd_create(c"racer-gpu-staging".as_ptr(), libc::MFD_CLOEXEC) };
        if fd < 0 {
            return None;
        }
        let file = unsafe { File::from_raw_fd(fd) };
        file.set_len(length as u64).ok()?;
        let pointer = unsafe {
            libc::mmap(
                std::ptr::null_mut(),
                length,
                libc::PROT_READ | libc::PROT_WRITE,
                libc::MAP_SHARED,
                std::os::fd::AsRawFd::as_raw_fd(&file),
                0,
            )
        };
        if pointer == libc::MAP_FAILED {
            return None;
        }
        Some(Self {
            file,
            pointer: std::ptr::NonNull::new(pointer.cast())?,
            length,
        })
    }
}

impl std::ops::Deref for Mapped {
    type Target = [u8];

    fn deref(&self) -> &[u8] {
        // SAFETY: the mapping lives as long as self. Other writers (a
        // simulated NIC) only write while the dataplane holds the slot.
        unsafe { std::slice::from_raw_parts(self.pointer.as_ptr(), self.length) }
    }
}

impl std::ops::DerefMut for Mapped {
    fn deref_mut(&mut self) -> &mut [u8] {
        // SAFETY: as for deref.
        unsafe { std::slice::from_raw_parts_mut(self.pointer.as_ptr(), self.length) }
    }
}

impl Drop for Mapped {
    fn drop(&mut self) {
        unsafe { libc::munmap(self.pointer.as_ptr().cast(), self.length) };
    }
}

/// One buffer.
struct Buffer {
    context: usize,

    size: u64,

    memory: Memory,
}

/// Everything the simulation tracks.
#[derive(Default)]
struct World {
    devices: Vec<SimDevice>,

    next: usize,

    contexts: BTreeMap<usize, SimContext>,

    streams: BTreeMap<usize, SimStream>,

    buffers: BTreeMap<usize, Buffer>,

    files: BTreeMap<usize, File>,

    /// memfd inode to device ordinal.
    allocations: BTreeMap<(u64, u64), i32>,

    /// (dst, src) ordinal pairs with no peer path. Others have one.
    no_peer: BTreeSet<(i32, i32)>,

    /// (dst context handle, src ordinal) pairs enabled by `peer`.
    peers: BTreeSet<(usize, i32)>,

    faults: VecDeque<(Operation, c_int)>,

    trace: Vec<Event>,

    initialized: bool,

    file_driver: bool,

    /// Whether the open that set `file_driver` asked for PCI P2PDMA.
    pci_p2pdma: bool,

    no_kernels: bool,

    last_error: i32,
}

impl Simulation {
    /// The world, locked. A panic on another thread does not poison it for
    /// the rest of a test.
    fn world(&self) -> MutexGuard<'_, World> {
        self.world
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// GPUs that discovery will report, in ordinal order.
    pub fn new(devices: Vec<SimDevice>) -> Self {
        Self {
            world: Arc::new(Mutex::new(World {
                devices,
                next: 0x1000,
                ..World::default()
            })),
        }
    }

    /// Whether the cuFile driver is open with PCI P2PDMA on. `None` before
    /// it opens.
    pub fn pci_p2pdma(&self) -> Option<bool> {
        let w = self.world();
        w.file_driver.then_some(w.pci_p2pdma)
    }

    /// Make this the simulation [`crate::Gpu::load`] uses on this thread.
    pub fn enter(&self) -> Environment {
        let previous = CURRENT.with(|current| current.borrow_mut().replace(self.clone()));
        Environment { previous }
    }

    /// A client allocation of `size` bytes on GPU `ordinal`, as the fd
    /// `cuMemExportToShareableHandle` would return. Keep a clone to read it.
    pub fn allocate(&self, ordinal: i32, size: u64) -> OwnedFd {
        let fd = unsafe { libc::memfd_create(c"racer-gpu-sim".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0, "memfd_create failed");
        let file = unsafe { File::from_raw_fd(fd) };
        file.set_len(size).expect("size memfd");
        let meta = file.metadata().expect("stat memfd");
        self.world()
            .allocations
            .insert((meta.dev(), meta.ino()), ordinal);
        file.into()
    }

    /// Fail the next call to `operation` with `code` (a `gpu_cuda.h` code;
    /// `-5` is a driver error). Nothing changes.
    pub fn fault(&self, operation: Operation, code: c_int) {
        self.world().faults.push_back((operation, code));
    }

    /// Whether GPU `dst` can read GPU `src`'s memory over a peer path. By
    /// default every pair can. Affects later `enable_peer` calls only.
    pub fn set_peer(&self, dst: i32, src: i32, allowed: bool) {
        let mut world = self.world();
        if allowed {
            world.no_peer.remove(&(dst, src));
        } else {
            world.no_peer.insert((dst, src));
        }
    }

    /// Behave like a shim built without decrypt kernels.
    pub fn disable_kernels(&self) {
        self.world().no_kernels = true;
    }

    /// Every call so far.
    pub fn trace(&self) -> Vec<Event> {
        self.world().trace.clone()
    }

    /// Live resources.
    pub fn live(&self) -> Live {
        let world = self.world();
        let (mut imports, mut staging) = (0, 0);
        for buffer in world.buffers.values() {
            match buffer.memory {
                Memory::Import { .. } => imports += 1,
                Memory::Staging { .. } => staging += 1,
            }
        }
        Live {
            contexts: world.contexts.len(),
            streams: world.streams.len(),
            imports,
            staging,
            files: world.files.len(),
        }
    }
}

/// The simulation in scope on this thread.
pub(crate) fn current() -> Option<Simulation> {
    CURRENT.with(|current| current.borrow().clone())
}

/// Run `f` on the current world, recording the call and applying a fault.
fn with<R: Into<i64> + Copy + From<i32>>(
    operation: Operation,
    f: impl FnOnce(&mut World) -> (R, usize),
) -> R {
    let Some(simulation) = current() else {
        return R::from(code::EUNAVAILABLE);
    };
    let mut world = simulation.world();
    let fault = world
        .faults
        .iter()
        .position(|(op, _)| *op == operation)
        .and_then(|i| world.faults.remove(i));
    let (result, handle) = match fault {
        Some((_, rc)) => {
            if rc == code::EDRIVER {
                world.last_error = 999;
            }
            if rc == code::EUNSUPPORTED {
                world.last_error = 5008; // CU_FILE_IO_NOT_SUPPORTED
            }
            (R::from(rc), 0)
        }
        None => f(&mut world),
    };
    world.trace.push(Event {
        operation,
        handle,
        result: result.into(),
    });
    result
}

/// Store a new handle.
fn give(world: &mut World, out: ffi::Out) -> usize {
    world.next += 0x10;
    unsafe { *out = std::ptr::without_provenance_mut(world.next) };
    world.next
}

/// Handle value of an opaque pointer.
fn id(pointer: *mut c_void) -> usize {
    pointer.addr()
}

/// Nonempty `[offset, offset + length)` inside `size`.
fn span(size: u64, offset: u64, length: u64) -> bool {
    length > 0 && offset <= size && length <= size - offset
}

unsafe extern "C" fn last_driver_error() -> i32 {
    current().map_or(0, |s| s.world().last_error)
}

unsafe extern "C" fn init() -> c_int {
    with(Operation::Init, |w| {
        w.initialized = true;
        (code::OK, 0)
    })
}

unsafe extern "C" fn discover(out: *mut RawDevice, capacity: u32) -> c_int {
    with(Operation::Discover, |w| {
        if !w.initialized {
            return (code::EUNAVAILABLE, 0);
        }
        for (i, device) in w.devices.iter().enumerate().take(capacity as usize) {
            let mut raw = RawDevice {
                uuid: device.uuid,
                ordinal: i as i32,
                numa_node: device.numa_node.map_or(-1, |n| n as i32),
                compute_mode: device.compute_mode,
                flags: device.flags,
                total_memory: device.total_memory,
                ..RawDevice::default()
            };
            let bytes = device.pci_bus_id.as_bytes();
            raw.pci_bus_id[..bytes.len().min(31)].copy_from_slice(&bytes[..bytes.len().min(31)]);
            unsafe { *out.add(i) = raw };
        }
        (w.devices.len() as c_int, 0)
    })
}

unsafe extern "C" fn context(ordinal: i32, out: ffi::Out) -> c_int {
    with(Operation::Context, |w| {
        if ordinal < 0 || ordinal as usize >= w.devices.len() {
            w.last_error = 101; // CUDA_ERROR_INVALID_DEVICE
            return (code::EDRIVER, 0);
        }
        let handle = give(w, out);
        w.contexts.insert(handle, SimContext { ordinal });
        (code::OK, handle)
    })
}

unsafe extern "C" fn context_free(context: *mut c_void) -> c_int {
    with(Operation::ContextFree, |w| {
        let handle = id(context);
        let used = w.streams.values().any(|s| s.context == handle)
            || w.buffers.values().any(|b| b.context == handle);
        if used {
            // Rust keeps the context alive while anything uses it; refuse
            // so a bug shows up as a counted leak.
            return (code::EBUSY, handle);
        }
        match w.contexts.remove(&handle) {
            Some(_) => (code::OK, handle),
            None => (code::EINVAL, handle),
        }
    })
}

unsafe extern "C" fn stream(context: *mut c_void, out: ffi::Out) -> c_int {
    with(Operation::Stream, |w| {
        if !w.contexts.contains_key(&id(context)) {
            return (code::EINVAL, 0);
        }
        let handle = give(w, out);
        w.streams.insert(
            handle,
            SimStream {
                context: id(context),
                decrypt: 0,
                verdict: RawVerdict::default(),
            },
        );
        (code::OK, handle)
    })
}

unsafe extern "C" fn stream_sync(stream: *mut c_void) -> c_int {
    with(Operation::StreamSync, |w| {
        match w.streams.get_mut(&id(stream)) {
            Some(s) => {
                if s.decrypt == 1 {
                    s.decrypt = 2;
                }
                (code::OK, id(stream))
            }
            None => (code::EINVAL, id(stream)),
        }
    })
}

unsafe extern "C" fn stream_free(stream: *mut c_void) -> c_int {
    with(Operation::StreamFree, |w| {
        match w.streams.remove(&id(stream)) {
            Some(_) => (code::OK, id(stream)),
            None => (code::EINVAL, id(stream)),
        }
    })
}

unsafe extern "C" fn import(context: *mut c_void, fd: c_int, size: u64, out: ffi::Out) -> c_int {
    with(Operation::Import, |w| {
        let Some(ctx) = w.contexts.get(&id(context)) else {
            return (code::EINVAL, 0);
        };
        let ordinal = ctx.ordinal;
        if fd < 0 || size == 0 {
            return (code::EINVAL, 0);
        }
        // Like cuMemImportFromShareableHandle, keep our own reference.
        let duplicate = unsafe { libc::fcntl(fd, libc::F_DUPFD_CLOEXEC, 0) };
        if duplicate < 0 {
            w.last_error = 1; // CUDA_ERROR_INVALID_VALUE
            return (code::EDRIVER, 0);
        }
        let file = unsafe { File::from_raw_fd(duplicate) };
        let Ok(meta) = file.metadata() else {
            return (code::EDRIVER, 0);
        };
        let Some(&owner) = w.allocations.get(&(meta.dev(), meta.ino())) else {
            w.last_error = 1;
            return (code::EDRIVER, 0);
        };
        if owner != ordinal {
            return (code::EDEVICE, 0);
        }
        if !size.is_multiple_of(GRANULARITY) {
            return (code::EINVAL, 0);
        }
        if size > meta.len() {
            w.last_error = 1;
            return (code::EDRIVER, 0);
        }
        let handle = give(w, out);
        w.buffers.insert(
            handle,
            Buffer {
                context: id(context),
                size,
                memory: Memory::Import { file },
            },
        );
        (code::OK, handle)
    })
}

unsafe extern "C" fn import_free(allocation: *mut c_void) -> c_int {
    with(Operation::ImportFree, |w| {
        let handle = id(allocation);
        match w.buffers.get(&handle) {
            Some(Buffer {
                memory: Memory::Import { .. },
                ..
            }) => {
                w.buffers.remove(&handle);
                (code::OK, handle)
            }
            _ => (code::EINVAL, handle),
        }
    })
}

unsafe extern "C" fn staging(context: *mut c_void, size: u64, out: ffi::Out) -> c_int {
    with(Operation::Staging, |w| {
        if !w.contexts.contains_key(&id(context)) || size == 0 {
            return (code::EINVAL, 0);
        }
        if size > 1 << 32 {
            return (code::ENOMEM, 0);
        }
        let Some(bytes) = Mapped::new(size as usize) else {
            return (code::ENOMEM, 0);
        };
        let handle = give(w, out);
        w.buffers.insert(
            handle,
            Buffer {
                context: id(context),
                size,
                memory: Memory::Staging {
                    bytes,
                    registered: false,
                },
            },
        );
        (code::OK, handle)
    })
}

unsafe extern "C" fn staging_free(staging: *mut c_void) -> c_int {
    with(Operation::StagingFree, |w| {
        let handle = id(staging);
        match w.buffers.get(&handle) {
            Some(Buffer {
                memory: Memory::Staging { registered, .. },
                ..
            }) => {
                if *registered {
                    return (code::EBUSY, handle);
                }
                w.buffers.remove(&handle);
                (code::OK, handle)
            }
            _ => (code::EINVAL, handle),
        }
    })
}

impl World {
    /// Check that a stream and buffer exist and share a context.
    fn pair(&self, stream: *mut c_void, buffer: *mut c_void) -> Result<(), c_int> {
        let s = self.streams.get(&id(stream)).ok_or(code::EINVAL)?;
        let b = self.buffers.get(&id(buffer)).ok_or(code::EINVAL)?;
        if s.context != b.context {
            return Err(code::EDEVICE);
        }
        Ok(())
    }

    /// Write bytes into a buffer, bounds already checked.
    fn write(&mut self, buffer: usize, offset: u64, bytes: &[u8]) -> c_int {
        match &mut self.buffers.get_mut(&buffer).expect("checked").memory {
            Memory::Import { file } => match file.write_all_at(bytes, offset) {
                Ok(()) => code::OK,
                Err(_) => code::EDRIVER,
            },
            Memory::Staging { bytes: memory, .. } => {
                memory[offset as usize..offset as usize + bytes.len()].copy_from_slice(bytes);
                code::OK
            }
        }
    }

    /// Staging bytes, or None for an import.
    fn staging(&self, buffer: usize) -> Option<&[u8]> {
        match &self.buffers.get(&buffer)?.memory {
            Memory::Staging { bytes, .. } => Some(bytes),
            Memory::Import { .. } => None,
        }
    }
}

unsafe extern "C" fn copy_h2d(
    stream: *mut c_void,
    dst: *mut c_void,
    offset: u64,
    src: *const c_void,
    length: u64,
) -> c_int {
    with(Operation::CopyH2d, |w| {
        if let Err(rc) = w.pair(stream, dst) {
            return (rc, id(dst));
        }
        if src.is_null() || !span(w.buffers[&id(dst)].size, offset, length) {
            return (code::ERANGE, id(dst));
        }
        let bytes = unsafe { std::slice::from_raw_parts(src.cast::<u8>(), length as usize) };
        (w.write(id(dst), offset, bytes), id(dst))
    })
}

unsafe extern "C" fn copy_d2d(
    stream: *mut c_void,
    dst: *mut c_void,
    dst_offset: u64,
    src: *mut c_void,
    src_offset: u64,
    length: u64,
) -> c_int {
    with(Operation::CopyD2d, |w| {
        if let Err(rc) = w.pair(stream, dst).and(w.pair(stream, src)) {
            return (rc, id(dst));
        }
        let Some(from) = w.staging(id(src)) else {
            return (code::EINVAL, id(dst));
        };
        if !span(w.buffers[&id(dst)].size, dst_offset, length)
            || !span(from.len() as u64, src_offset, length)
        {
            return (code::ERANGE, id(dst));
        }
        let bytes = from[src_offset as usize..(src_offset + length) as usize].to_vec();
        (w.write(id(dst), dst_offset, &bytes), id(dst))
    })
}

unsafe extern "C" fn peer(dst: *mut c_void, src: *mut c_void, access: *mut c_int) -> c_int {
    with(Operation::Peer, |w| {
        let (Some(d), Some(from)) = (w.contexts.get(&id(dst)), w.contexts.get(&id(src))) else {
            return (code::EINVAL, id(dst));
        };
        if access.is_null() {
            return (code::EINVAL, id(dst));
        }
        let (d, from) = (d.ordinal, from.ordinal);
        let allowed = d == from || !w.no_peer.contains(&(d, from));
        if allowed {
            w.peers.insert((id(dst), from));
        }
        unsafe { *access = allowed as c_int };
        (code::OK, id(dst))
    })
}

unsafe extern "C" fn copy_peer(
    stream: *mut c_void,
    dst: *mut c_void,
    dst_offset: u64,
    src: *mut c_void,
    src_offset: u64,
    length: u64,
) -> c_int {
    with(Operation::CopyPeer, |w| {
        if let Err(rc) = w.pair(stream, dst) {
            return (rc, id(dst));
        }
        let Some(b) = w.buffers.get(&id(src)) else {
            return (code::EINVAL, id(dst));
        };
        let Some(from) = w.staging(id(src)) else {
            return (code::EINVAL, id(dst));
        };
        let context = w.streams[&id(stream)].context;
        if b.context == context || !w.peers.contains(&(context, w.contexts[&b.context].ordinal)) {
            return (code::EDEVICE, id(dst));
        }
        if !span(w.buffers[&id(dst)].size, dst_offset, length)
            || !span(from.len() as u64, src_offset, length)
        {
            return (code::ERANGE, id(dst));
        }
        let bytes = from[src_offset as usize..(src_offset + length) as usize].to_vec();
        (w.write(id(dst), dst_offset, &bytes), id(dst))
    })
}

unsafe extern "C" fn copy_d2h(
    stream: *mut c_void,
    dst: *mut c_void,
    src: *mut c_void,
    src_offset: u64,
    length: u64,
) -> c_int {
    with(Operation::CopyD2h, |w| {
        if let Err(rc) = w.pair(stream, src) {
            return (rc, id(src));
        }
        let Some(from) = w.staging(id(src)) else {
            return (code::EINVAL, id(src));
        };
        if dst.is_null() || !span(from.len() as u64, src_offset, length) {
            return (code::ERANGE, id(src));
        }
        let out = unsafe { std::slice::from_raw_parts_mut(dst.cast::<u8>(), length as usize) };
        out.copy_from_slice(&from[src_offset as usize..(src_offset + length) as usize]);
        let s = w.streams.get_mut(&id(stream)).expect("checked");
        if s.decrypt == 1 {
            s.decrypt = 2;
        }
        (code::OK, id(src))
    })
}

unsafe extern "C" fn decrypt(
    stream: *mut c_void,
    params: *const RawDecrypt,
    src: *mut c_void,
    src_offset: u64,
    dst: *mut c_void,
    dst_offset: u64,
) -> c_int {
    with(Operation::Decrypt, |w| {
        if params.is_null() {
            return (code::EINVAL, 0);
        }
        let p = unsafe { &*params };
        if let Err(rc) = w.pair(stream, dst).and(w.pair(stream, src)) {
            return (rc, id(dst));
        }
        if p.length > ffi::MAX_DECRYPT
            || p.aad_length > ffi::MAX_AAD as u64
            || (p.aad.is_null() && p.aad_length > 0)
            || p.check_crc > 1
        {
            return (code::EINVAL, id(dst));
        }
        let Some(from) = w.staging(id(src)) else {
            return (code::EINVAL, id(dst));
        };
        let n = p.length;
        let dst_size = w.buffers[&id(dst)].size;
        if !span(from.len() as u64, src_offset, n + 16)
            || (n > 0 && !span(dst_size, dst_offset, n))
            || (n == 0 && dst_offset > dst_size)
        {
            return (code::ERANGE, id(dst));
        }
        if w.streams[&id(stream)].decrypt == 1 {
            return (code::EBUSY, id(dst));
        }
        if w.no_kernels {
            return (code::ENOKERNEL, id(dst));
        }
        let sealed = &from[src_offset as usize..(src_offset + n + 16) as usize];
        let aad = if p.aad_length == 0 {
            &[][..]
        } else {
            unsafe { std::slice::from_raw_parts(p.aad, p.aad_length as usize) }
        };
        let mut out = zeroize::Zeroizing::new(vec![0; n as usize]);
        let expected = (p.check_crc == 1).then_some(p.expected_crc);
        let Some(verdict) = gcm::decrypt(&p.key, &p.nonce, aad, sealed, expected, &mut out) else {
            return (code::ENOMEM, id(dst));
        };
        let mut rc = code::OK;
        if verdict.status == gcm::VERDICT_PASS && n > 0 {
            rc = w.write(id(dst), dst_offset, &out);
        }
        let s = w.streams.get_mut(&id(stream)).expect("checked");
        s.decrypt = 1;
        s.verdict = verdict;
        (rc, id(dst))
    })
}

unsafe extern "C" fn verdict(stream: *mut c_void, out: *mut RawVerdict) -> c_int {
    with(Operation::Verdict, |w| {
        let Some(s) = w.streams.get_mut(&id(stream)) else {
            return (code::EINVAL, 0);
        };
        if s.decrypt != 2 || out.is_null() {
            return (code::EBUSY, id(stream));
        }
        unsafe { *out = std::mem::take(&mut s.verdict) };
        s.decrypt = 0;
        (code::OK, id(stream))
    })
}

unsafe extern "C" fn file_open(flags: u32) -> c_int {
    with(Operation::FileOpen, |w| {
        // As in the shim, flags apply only to the open that loads cuFile.
        if !w.file_driver {
            w.pci_p2pdma = flags & ffi::file_flag::PCI_P2PDMA != 0;
        }
        w.file_driver = true;
        (code::OK, 0)
    })
}

unsafe extern "C" fn file(fd: c_int, out: ffi::Out) -> c_int {
    with(Operation::File, |w| {
        if !w.file_driver {
            return (code::EUNAVAILABLE, 0);
        }
        if fd < 0 || unsafe { libc::fcntl(fd, libc::F_GETFD) } < 0 {
            return (code::EINVAL, 0);
        }
        // cuFile reads with its own alignment rules. Reopen the same file
        // without O_DIRECT so host reads of any length and buffer work.
        let Ok(reopened) = File::open(format!("/proc/self/fd/{fd}")) else {
            return (code::EINVAL, 0);
        };
        let handle = give(w, out);
        w.files.insert(handle, reopened);
        (code::OK, handle)
    })
}

unsafe extern "C" fn file_free(file: *mut c_void) -> c_int {
    with(Operation::FileFree, |w| match w.files.remove(&id(file)) {
        Some(_) => (code::OK, id(file)),
        None => (code::EINVAL, id(file)),
    })
}

/// Set a staging buffer's cuFile registration.
fn register(w: &mut World, staging: *mut c_void, value: bool) -> (c_int, usize) {
    if !w.file_driver {
        return (code::EUNAVAILABLE, id(staging));
    }
    match w.buffers.get_mut(&id(staging)).map(|b| &mut b.memory) {
        Some(Memory::Staging { registered, .. }) if *registered != value => {
            *registered = value;
            (code::OK, id(staging))
        }
        Some(Memory::Staging { .. }) => (code::EBUSY, id(staging)),
        _ => (code::EINVAL, id(staging)),
    }
}

unsafe extern "C" fn staging_register(staging: *mut c_void) -> c_int {
    with(Operation::StagingRegister, |w| register(w, staging, true))
}

unsafe extern "C" fn staging_unregister(staging: *mut c_void) -> c_int {
    with(Operation::StagingUnregister, |w| {
        register(w, staging, false)
    })
}

unsafe extern "C" fn file_read(
    file: *mut c_void,
    staging: *mut c_void,
    offset: u64,
    file_offset: u64,
    length: u64,
) -> i64 {
    with(Operation::FileRead, |w| {
        let Some(source) = w.files.get(&id(file)) else {
            return (code::EINVAL as i64, 0);
        };
        let Some(Buffer {
            memory: Memory::Staging { bytes, .. },
            size,
            ..
        }) = w.buffers.get_mut(&id(staging))
        else {
            return (code::EINVAL as i64, 0);
        };
        if !span(*size, offset, length) {
            return (code::ERANGE as i64, id(staging));
        }
        let out = &mut bytes[offset as usize..(offset + length) as usize];
        match source.read_exact_at(out, file_offset) {
            Ok(()) => (length as i64, id(staging)),
            Err(_) => (code::EIO as i64, id(staging)),
        }
    })
}

unsafe extern "C" fn dmabuf(
    staging: *mut c_void,
    offset: u64,
    length: u64,
    fd: *mut c_int,
) -> c_int {
    with(Operation::Dmabuf, |w| {
        let Some(Buffer {
            memory: Memory::Staging { bytes, .. },
            ..
        }) = w.buffers.get(&id(staging))
        else {
            return (code::EINVAL, id(staging));
        };
        let size = bytes.len() as u64;
        let live = (offset == 0).then(|| bytes.file.try_clone());
        if fd.is_null() {
            return (code::EINVAL, id(staging));
        }
        let page = 4096;
        if length == 0
            || !offset.is_multiple_of(page)
            || !length.is_multiple_of(page)
            || offset + length > size
        {
            return (code::ERANGE, id(staging));
        }
        // An export from offset 0 is a live view: a duplicate of the backing
        // memfd, so the dma-buf offset equals the staging offset as it does on
        // real hardware. Other offsets get a placeholder fd of the right size.
        if let Some(file) = live {
            let Ok(file) = file else {
                return (code::EDRIVER, id(staging));
            };
            unsafe { *fd = std::os::fd::IntoRawFd::into_raw_fd(file) };
            return (code::OK, id(staging));
        }
        let memfd = unsafe { libc::memfd_create(c"racer-gpu-dmabuf".as_ptr(), libc::MFD_CLOEXEC) };
        if memfd < 0 {
            return (code::EDRIVER, id(staging));
        }
        let file = unsafe { File::from_raw_fd(memfd) };
        if file.set_len(length).is_err() {
            return (code::EDRIVER, id(staging));
        }
        unsafe { *fd = std::os::fd::IntoRawFd::into_raw_fd(file) };
        (code::OK, id(staging))
    })
}

/// The simulation's function table.
pub(crate) fn api() -> Rc<Api> {
    Rc::new(Api {
        library: None,
        last_driver_error,
        init,
        discover,
        context,
        context_free,
        stream,
        stream_sync,
        stream_free,
        import,
        import_free,
        staging,
        staging_free,
        copy_h2d,
        copy_d2d,
        peer,
        copy_peer,
        copy_d2h,
        decrypt,
        verdict,
        file_open,
        file,
        file_free,
        staging_register,
        staging_unregister,
        file_read,
        dmabuf,
    })
}

/// Read `length` bytes at `offset` of a client allocation fd.
pub fn read_allocation(fd: &OwnedFd, offset: u64, length: usize) -> Vec<u8> {
    let file = File::from(fd.try_clone().expect("dup"));
    let mut out = vec![0; length];
    file.read_exact_at(&mut out, offset).expect("read memfd");
    out
}
