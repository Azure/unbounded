//! Fake GPUs for tests. No CUDA, driver or hardware needed.
//!
//! It implements the same C functions as `native/gpu_cuda.c`, so the real
//! wrappers in `lib.rs` run on top of it unchanged. Device memory is host
//! memory; a client allocation is a memfd made with
//! [`Simulation::allocate`] and passed as the shareable handle. Decryption
//! runs the serial host emulation of the kernels (`native/gcm_host.c`).
//!
//! Queued work is deferred by default, as on a real GPU: copies and decrypts
//! run when their stream synchronizes, when an event recorded after them is
//! queried or synchronized, or when a stream that waits on such an event
//! runs. Deferred mode fails loudly (panics) when a caller breaks the async
//! contract: a source changed after its copy or decrypt was queued (the
//! decrypt kernels read ciphertext twice, so this would let unauthenticated
//! bytes through), or a buffer or host allocation was freed while queued
//! work still uses it. [`Simulation::set_immediate`] runs work when queued
//! instead. Tests check ordering with [`Simulation::trace`] instead of timing.
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
    sync::{Arc, Condvar, Mutex, MutexGuard},
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
    Event,
    EventFree,
    EventRecord,
    EventQuery,
    EventSync,
    StreamWait,
    HostAlloc,
    HostFree,
    HostRegister,
    HostUnregister,
    CopyD2hAsync,
    FlushRdma,
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
    pub events: usize,
    pub host: usize,
}

/// A simulated set of GPUs, shared by clones. Deterministic.
#[derive(Clone)]
pub struct Simulation {
    world: Arc<Mutex<World>>,

    /// Wakes calls waiting at a [`Simulation::hold`].
    gate: Arc<Condvar>,
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

/// A stream, its queued work and its last verdict.
struct SimStream {
    context: usize,

    /// 0 none, 1 queued, 2 complete, as in the shim.
    decrypt: u8,

    verdict: RawVerdict,

    /// Work queued and not yet run (deferred mode).
    queue: VecDeque<Pending>,

    /// Items ever queued; an event records this count.
    queued: u64,

    /// Items run so far.
    done: u64,

    /// `queued` right after the last decrypt.
    decrypt_seq: u64,

    /// First failure of deferred work, reported by the next sync.
    error: c_int,
}

impl SimStream {
    fn new(context: usize) -> Self {
        Self {
            context,
            decrypt: 0,
            verdict: RawVerdict::default(),
            queue: VecDeque::new(),
            queued: 0,
            done: 0,
            decrypt_seq: 0,
            error: code::OK,
        }
    }
}

/// Deferred work run by its stream.
type Work = Box<dyn FnOnce(&mut World) -> c_int + Send>;

/// One item in a stream's queue.
enum Pending {
    /// Run `work` after checking the sources did not change since queueing.
    Run {
        work: Work,
        checks: Vec<Check>,
        /// Buffers and host allocations the work touches.
        uses: Vec<usize>,
    },
    /// Run stream `stream` up to item `seq` first (cuStreamWaitEvent).
    Wait { stream: usize, seq: u64 },
}

/// A source range and its hash when the work was queued.
struct Check {
    what: &'static str,
    source: Source,
    hash: u64,
}

/// Bytes a queued item reads.
#[derive(Clone, Copy)]
enum Source {
    Host(HostPtr, usize),
    Staging {
        buffer: usize,
        offset: u64,
        length: u64,
    },
}

/// A host pointer queued work reads or writes. The Rust wrappers keep the
/// memory alive until the work completes; deferred mode checks that.
#[derive(Clone, Copy)]
struct HostPtr(*mut u8);

// SAFETY: only dereferenced under the world's lock, while the caller keeps
// the memory alive per the async contract that deferred mode checks.
unsafe impl Send for HostPtr {}

impl HostPtr {
    /// The pointer. A method, so closures capture the `Send` wrapper.
    fn get(self) -> *mut u8 {
        self.0
    }
}

/// Whether a write to `a` may change bytes `b` reads.
fn overlaps(a: Source, b: Source) -> bool {
    let range = |source| match source {
        Source::Host(pointer, length) => (0usize, pointer.get().addr() as u64, length as u64),
        Source::Staging {
            buffer,
            offset,
            length,
        } => (buffer, offset, length),
    };
    let ((ka, sa, la), (kb, sb, lb)) = (range(a), range(b));
    ka == kb && sa < sb + lb && sb < sa + la
}

/// A recorded event.
struct SimEvent {
    context: usize,
    /// The stream and item count captured by the last record.
    mark: Option<(usize, u64)>,
    /// Queries answered "not ready" so far for the current mark.
    polls: u32,
}

/// Page-locked host memory from `host_alloc`.
struct HostMemory {
    bytes: Box<[u8]>,
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

    /// Run queued work when queued instead of deferring it.
    immediate: bool,

    /// Queries of a pending event that answer "not ready" before the work
    /// runs, so callers exercise their polling paths.
    query_delay: u32,

    events: BTreeMap<usize, SimEvent>,

    /// `host_alloc` memory by address.
    host: BTreeMap<usize, HostMemory>,

    /// `host_register` ranges by address.
    registered: BTreeMap<usize, u64>,

    /// `CU_DEVICE_ATTRIBUTE_GPU_DIRECT_RDMA_WRITES_ORDERING` of every GPU.
    rdma_ordering: u32,

    /// Whether cuFlushGPUDirectRDMAWrites is available.
    rdma_flush: bool,

    /// Flushes `flush_rdma` performed.
    rdma_flushes: u64,

    /// Ranges written by the work item now running. Queued items that read
    /// them are rehashed: in-order GPU writes are legitimate.
    written: Vec<Source>,

    /// Async-contract violations deferred mode found and nobody took yet.
    violations: Violations,

    /// Operations whose calls wait for a permit before running.
    held: Vec<Operation>,

    /// One entry per call [`Simulation::admit`] let through and none took yet.
    permits: Vec<Operation>,

    /// One entry per call waiting at a hold.
    waiting: Vec<Operation>,
}

/// Recorded async-contract violations.
#[derive(Default)]
struct Violations(Vec<String>);

impl Drop for Violations {
    /// Fail loudly if a violation went unchecked. Shim functions cannot
    /// unwind (they are `extern "C"`), so violations are recorded there and
    /// raised here, when the simulation drops, or taken by
    /// [`Simulation::take_violations`].
    fn drop(&mut self) {
        if !self.0.is_empty() && !std::thread::panicking() {
            panic!("simulated GPU async-contract violations: {:?}", self.0);
        }
    }
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
                query_delay: 1,
                rdma_flush: true,
                ..World::default()
            })),
            gate: Arc::new(Condvar::new()),
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

    /// Make calls to `operation` wait (`true`) until [`Simulation::admit`]
    /// lets each one through, or let every call run (`false`, the default).
    /// A waiting call blocks its thread but not the simulation, so tests can
    /// hold work in flight until other threads reach a known state.
    pub fn hold(&self, operation: Operation, held: bool) {
        let mut world = self.world();
        world.held.retain(|op| *op != operation);
        if held {
            world.held.push(operation);
        } else {
            world.permits.retain(|op| *op != operation);
        }
        drop(world);
        self.gate.notify_all();
    }

    /// Let one held call to `operation`, waiting now or made later, run.
    pub fn admit(&self, operation: Operation) {
        self.world().permits.push(operation);
        self.gate.notify_all();
    }

    /// Calls to `operation` waiting at a hold now.
    pub fn waiting(&self, operation: Operation) -> usize {
        self.world()
            .waiting
            .iter()
            .filter(|op| **op == operation)
            .count()
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

    /// Run queued work when it is queued (`true`) or defer it until a sync
    /// or event (`false`, the default).
    pub fn set_immediate(&self, immediate: bool) {
        self.world().immediate = immediate;
    }

    /// Queries of a pending event that answer "not ready" before its work
    /// runs. Default 1.
    pub fn set_query_delay(&self, polls: u32) {
        self.world().query_delay = polls;
    }

    /// GPUDirect RDMA write ordering the GPUs report (0, 100 or 200), and
    /// whether cuFlushGPUDirectRDMAWrites is available. Default: no native
    /// ordering, flush available.
    pub fn set_rdma_ordering(&self, ordering: u32, flush: bool) {
        let mut world = self.world();
        world.rdma_ordering = ordering;
        world.rdma_flush = flush;
    }

    /// Async-contract violations found so far (a source changed after its
    /// work was queued, or memory was freed while queued work used it), and
    /// clear them. Unchecked violations panic when the simulation drops.
    pub fn take_violations(&self) -> Vec<String> {
        std::mem::take(&mut self.world().violations.0)
    }

    /// cuFlushGPUDirectRDMAWrites calls so far.
    pub fn rdma_flushes(&self) -> u64 {
        self.world().rdma_flushes
    }

    /// Write `bytes` at `offset` of a staging buffer as a NIC or another
    /// agent would, outside any stream. For tests of the async contract.
    pub fn poke_staging(&self, ordinal: i32, offset: u64, bytes: &[u8]) {
        let mut world = self.world();
        let World {
            buffers, contexts, ..
        } = &mut *world;
        for buffer in buffers.values_mut() {
            if contexts.get(&buffer.context).map(|c| c.ordinal) != Some(ordinal) {
                continue;
            }
            if let Memory::Staging { bytes: memory, .. } = &mut buffer.memory
                && offset as usize + bytes.len() <= memory.len()
            {
                memory[offset as usize..offset as usize + bytes.len()].copy_from_slice(bytes);
            }
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
            events: world.events.len(),
            host: world.host.len(),
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
    if world.held.contains(&operation) {
        world.waiting.push(operation);
        while world.held.contains(&operation) {
            if let Some(permit) = world.permits.iter().position(|op| *op == operation) {
                world.permits.remove(permit);
                break;
            }
            world = simulation
                .gate
                .wait(world)
                .unwrap_or_else(|poisoned| poisoned.into_inner());
        }
        if let Some(waiting) = world.waiting.iter().position(|op| *op == operation) {
            world.waiting.remove(waiting);
        }
    }
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
        w.streams.insert(handle, SimStream::new(id(context)));
        (code::OK, handle)
    })
}

unsafe extern "C" fn stream_sync(stream: *mut c_void) -> c_int {
    with(Operation::StreamSync, |w| {
        let handle = id(stream);
        let Some(upto) = w.streams.get(&handle).map(|s| s.queued) else {
            return (code::EINVAL, handle);
        };
        w.run(handle, upto);
        let s = w.streams.get_mut(&handle).expect("checked");
        if s.decrypt == 1 {
            s.decrypt = 2;
        }
        (std::mem::replace(&mut s.error, code::OK), handle)
    })
}

unsafe extern "C" fn stream_free(stream: *mut c_void) -> c_int {
    with(Operation::StreamFree, |w| {
        let handle = id(stream);
        // Like the shim, synchronize first.
        let Some(upto) = w.streams.get(&handle).map(|s| s.queued) else {
            return (code::EINVAL, handle);
        };
        w.run(handle, upto);
        w.streams.remove(&handle);
        (code::OK, handle)
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
                w.check_unused(handle, "client import");
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
                w.check_unused(handle, "staging buffer");
                w.buffers.remove(&handle);
                (code::OK, handle)
            }
            _ => (code::EINVAL, handle),
        }
    })
}

impl World {
    /// Queue `work` on `stream`, or run it now in immediate mode. Returns
    /// the result to give the caller: queue-time checks already passed, so
    /// deferred work reports failures at the next sync.
    fn enqueue(
        &mut self,
        stream: usize,
        sources: &[(&'static str, Source)],
        uses: Vec<usize>,
        work: Work,
    ) -> c_int {
        if self.immediate {
            let rc = work(self);
            self.written.clear();
            return rc;
        }
        let checks = sources
            .iter()
            .map(|&(what, source)| Check {
                what,
                source,
                hash: self.hash(source),
            })
            .collect();
        let s = self.streams.get_mut(&stream).expect("checked");
        s.queue.push_back(Pending::Run { work, checks, uses });
        s.queued += 1;
        code::OK
    }

    /// Hash of a source's current bytes.
    fn hash(&self, source: Source) -> u64 {
        use std::hash::{Hash, Hasher};
        let mut hasher = std::hash::DefaultHasher::new();
        match source {
            Source::Host(pointer, length) => {
                // SAFETY: the caller keeps queued host sources alive.
                unsafe { std::slice::from_raw_parts(pointer.get(), length) }.hash(&mut hasher)
            }
            Source::Staging {
                buffer,
                offset,
                length,
            } => {
                if let Some(bytes) = self.staging(buffer) {
                    bytes[offset as usize..(offset + length) as usize].hash(&mut hasher);
                }
            }
        }
        hasher.finish()
    }

    /// Run `stream`'s queued work up to item `upto`.
    fn run(&mut self, stream: usize, upto: u64) {
        loop {
            let Some(s) = self.streams.get_mut(&stream) else {
                return;
            };
            if s.done >= upto {
                return;
            }
            let Some(item) = s.queue.pop_front() else {
                return;
            };
            s.done += 1;
            match item {
                Pending::Wait { stream: other, seq } => self.run(other, seq),
                Pending::Run { work, checks, .. } => {
                    let mut rc = code::OK;
                    for check in &checks {
                        if self.hash(check.source) != check.hash {
                            self.violation(format!(
                                "{} changed after work reading it was queued",
                                check.what
                            ));
                            rc = code::EDRIVER;
                        }
                    }
                    if rc == code::OK {
                        rc = work(self);
                    }
                    self.rehash_written();
                    if let Some(s) = self.streams.get_mut(&stream)
                        && rc != code::OK
                        && s.error == code::OK
                    {
                        s.error = rc;
                    }
                }
            }
        }
    }

    /// Record a broken async contract.
    fn violation(&mut self, message: String) {
        eprintln!("simulated GPU: {message}");
        self.last_error = 0xbad;
        self.violations.0.push(message);
    }

    /// Refresh the hashes of queued reads of ranges the last item wrote.
    fn rehash_written(&mut self) {
        let written = std::mem::take(&mut self.written);
        if written.is_empty() {
            return;
        }
        let mut stale = Vec::new();
        for (stream, s) in &self.streams {
            for (index, item) in s.queue.iter().enumerate() {
                if let Pending::Run { checks, .. } = item {
                    for (c, check) in checks.iter().enumerate() {
                        if written.iter().any(|w| overlaps(*w, check.source)) {
                            stale.push((*stream, index, c));
                        }
                    }
                }
            }
        }
        for (stream, index, c) in stale {
            let source = match &self.streams[&stream].queue[index] {
                Pending::Run { checks, .. } => checks[c].source,
                Pending::Wait { .. } => continue,
            };
            let hash = self.hash(source);
            if let Some(Pending::Run { checks, .. }) = self
                .streams
                .get_mut(&stream)
                .and_then(|s| s.queue.get_mut(index))
            {
                checks[c].hash = hash;
            }
        }
    }

    /// Record a violation if queued work still uses buffer or host
    /// allocation `handle`.
    fn check_unused(&mut self, handle: usize, what: &str) {
        let used = self.streams.values().any(|s| {
            s.queue
                .iter()
                .any(|item| matches!(item, Pending::Run { uses, .. } if uses.contains(&handle)))
        });
        if used {
            self.violation(format!(
                "{what} {handle:#x} freed while queued work uses it"
            ));
        }
    }

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
        self.written.push(Source::Staging {
            buffer,
            offset,
            length: bytes.len() as u64,
        });
        let Some(target) = self.buffers.get_mut(&buffer) else {
            return code::EINVAL;
        };
        match &mut target.memory {
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
        let source = HostPtr(src.cast::<u8>().cast_mut());
        let (target, n) = (id(dst), length as usize);
        let rc = w.enqueue(
            id(stream),
            &[("host source", Source::Host(source, n))],
            vec![target, src.addr()],
            Box::new(move |w: &mut World| {
                // SAFETY: the caller keeps the source alive until completion.
                let bytes = unsafe { std::slice::from_raw_parts(source.get(), n) };
                w.write(target, offset, bytes)
            }),
        );
        (rc, id(dst))
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
        let (target, source) = (id(dst), id(src));
        let rc = w.enqueue(
            id(stream),
            &[(
                "staging source",
                Source::Staging {
                    buffer: source,
                    offset: src_offset,
                    length,
                },
            )],
            vec![target, source],
            Box::new(move |w: &mut World| {
                let Some(from) = w.staging(source) else {
                    return code::EINVAL;
                };
                let bytes = from[src_offset as usize..(src_offset + length) as usize].to_vec();
                w.write(target, dst_offset, &bytes)
            }),
        );
        (rc, id(dst))
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
        let (target, source) = (id(dst), id(src));
        let rc = w.enqueue(
            id(stream),
            &[(
                "staging source",
                Source::Staging {
                    buffer: source,
                    offset: src_offset,
                    length,
                },
            )],
            vec![target, source],
            Box::new(move |w: &mut World| {
                let Some(from) = w.staging(source) else {
                    return code::EINVAL;
                };
                let bytes = from[src_offset as usize..(src_offset + length) as usize].to_vec();
                w.write(target, dst_offset, &bytes)
            }),
        );
        (rc, id(dst))
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
        // Synchronous: earlier work on the stream runs first.
        let upto = w.streams[&id(stream)].queued;
        w.run(id(stream), upto);
        let Some(from) = w.staging(id(src)) else {
            return (code::EINVAL, id(src));
        };
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
            || p.reserved != 0
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
        let _ = from;
        // The shim copies the key schedule and AAD terms before returning.
        let aad = if p.aad_length == 0 {
            Vec::new()
        } else {
            unsafe { std::slice::from_raw_parts(p.aad, p.aad_length as usize) }.to_vec()
        };
        let key = zeroize::Zeroizing::new(p.key);
        let nonce = zeroize::Zeroizing::new(p.nonce);
        let (handle, source, target) = (id(stream), id(src), id(dst));
        let s = w.streams.get_mut(&handle).expect("checked");
        s.decrypt = 1;
        s.verdict = RawVerdict::default();
        let rc = w.enqueue(
            handle,
            &[(
                "decrypt source (read twice: GHASH, then CTR)",
                Source::Staging {
                    buffer: source,
                    offset: src_offset,
                    length: n + 16,
                },
            )],
            vec![source, target],
            Box::new(move |w: &mut World| {
                let Some(from) = w.staging(source) else {
                    return code::EINVAL;
                };
                let sealed = &from[src_offset as usize..(src_offset + n + 16) as usize];
                let mut out = zeroize::Zeroizing::new(vec![0; n as usize]);
                let Some(verdict) = gcm::decrypt(&key, &nonce, &aad, sealed, &mut out) else {
                    return code::ENOMEM;
                };
                let mut rc = code::OK;
                if verdict.status == gcm::VERDICT_PASS && n > 0 {
                    rc = w.write(target, dst_offset, &out);
                }
                if let Some(s) = w.streams.get_mut(&handle) {
                    s.verdict = verdict;
                }
                rc
            }),
        );
        let s = w.streams.get_mut(&handle).expect("checked");
        s.decrypt_seq = s.queued;
        (rc, target)
    })
}

unsafe extern "C" fn verdict(stream: *mut c_void, out: *mut RawVerdict) -> c_int {
    with(Operation::Verdict, |w| {
        let Some(s) = w.streams.get_mut(&id(stream)) else {
            return (code::EINVAL, 0);
        };
        if s.decrypt == 1 && s.done >= s.decrypt_seq {
            s.decrypt = 2;
        }
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

unsafe extern "C" fn event(context: *mut c_void, out: ffi::Out) -> c_int {
    with(Operation::Event, |w| {
        if !w.contexts.contains_key(&id(context)) || out.is_null() {
            return (code::EINVAL, 0);
        }
        let handle = give(w, out);
        w.events.insert(
            handle,
            SimEvent {
                context: id(context),
                mark: None,
                polls: 0,
            },
        );
        (code::OK, handle)
    })
}

impl World {
    /// Run the work an event captured. Returns false for an unknown event.
    fn complete(&mut self, event: usize) -> bool {
        let Some(e) = self.events.get(&event) else {
            return false;
        };
        if let Some((stream, seq)) = e.mark {
            self.run(stream, seq);
        }
        true
    }

    /// The deferred failure of the stream an event marks, taken so it is
    /// reported once, like a stream sync reports it. A real GPU reports a
    /// failed launch or copy through every later query of the context.
    fn event_error(&mut self, event: usize) -> c_int {
        let Some((stream, _)) = self.events.get(&event).and_then(|e| e.mark) else {
            return code::OK;
        };
        self.streams
            .get_mut(&stream)
            .map_or(code::OK, |s| std::mem::replace(&mut s.error, code::OK))
    }

    /// Whether the event's captured work has run.
    fn reached(&self, event: usize) -> bool {
        match self.events.get(&event).and_then(|e| e.mark) {
            Some((stream, seq)) => self.streams.get(&stream).is_none_or(|s| s.done >= seq),
            None => true,
        }
    }
}

unsafe extern "C" fn event_free(event: *mut c_void) -> c_int {
    with(Operation::EventFree, |w| {
        if !w.complete(id(event)) {
            return (code::EINVAL, id(event));
        }
        w.events.remove(&id(event));
        (code::OK, id(event))
    })
}

unsafe extern "C" fn event_record(event: *mut c_void, stream: *mut c_void) -> c_int {
    with(Operation::EventRecord, |w| {
        let (Some(e), Some(s)) = (w.events.get(&id(event)), w.streams.get(&id(stream))) else {
            return (code::EINVAL, id(event));
        };
        if e.context != s.context {
            return (code::EDEVICE, id(event));
        }
        let mark = (id(stream), s.queued);
        let e = w.events.get_mut(&id(event)).expect("checked");
        e.mark = Some(mark);
        e.polls = 0;
        (code::OK, id(event))
    })
}

unsafe extern "C" fn event_query(event: *mut c_void) -> c_int {
    with(Operation::EventQuery, |w| {
        let handle = id(event);
        if !w.events.contains_key(&handle) {
            return (code::EINVAL, handle);
        }
        if w.reached(handle) {
            return (w.event_error(handle), handle);
        }
        let delay = w.query_delay;
        let e = w.events.get_mut(&handle).expect("checked");
        if e.polls < delay {
            e.polls += 1;
            return (code::EBUSY, handle);
        }
        w.complete(handle);
        (w.event_error(handle), handle)
    })
}

unsafe extern "C" fn event_sync(event: *mut c_void) -> c_int {
    with(Operation::EventSync, |w| {
        if !w.complete(id(event)) {
            return (code::EINVAL, id(event));
        }
        (w.event_error(id(event)), id(event))
    })
}

unsafe extern "C" fn stream_wait(stream: *mut c_void, event: *mut c_void) -> c_int {
    with(Operation::StreamWait, |w| {
        let Some(e) = w.events.get(&id(event)) else {
            return (code::EINVAL, id(stream));
        };
        let mark = e.mark;
        let immediate = w.immediate;
        let Some(s) = w.streams.get_mut(&id(stream)) else {
            return (code::EINVAL, id(stream));
        };
        if let Some((other, seq)) = mark {
            if immediate {
                w.run(other, seq);
            } else {
                s.queue.push_back(Pending::Wait { stream: other, seq });
                s.queued += 1;
            }
        }
        (code::OK, id(stream))
    })
}

unsafe extern "C" fn host_alloc(context: *mut c_void, size: u64, out: ffi::Out) -> c_int {
    with(Operation::HostAlloc, |w| {
        if !w.contexts.contains_key(&id(context)) || size == 0 || out.is_null() {
            return (code::EINVAL, 0);
        }
        if size > 1 << 32 {
            return (code::ENOMEM, 0);
        }
        let mut bytes = vec![0u8; size as usize].into_boxed_slice();
        let pointer = bytes.as_mut_ptr();
        w.host.insert(pointer.addr(), HostMemory { bytes });
        unsafe { *out = pointer.cast() };
        (code::OK, pointer.addr())
    })
}

unsafe extern "C" fn host_free(context: *mut c_void, host: *mut c_void) -> c_int {
    with(Operation::HostFree, |w| {
        if !w.contexts.contains_key(&id(context)) || !w.host.contains_key(&host.addr()) {
            return (code::EINVAL, host.addr());
        }
        w.check_unused(host.addr(), "pinned host buffer");
        let memory = w.host.remove(&host.addr()).expect("checked");
        drop(memory.bytes);
        (code::OK, host.addr())
    })
}

unsafe extern "C" fn host_register(context: *mut c_void, host: *mut c_void, size: u64) -> c_int {
    with(Operation::HostRegister, |w| {
        if !w.contexts.contains_key(&id(context)) || host.is_null() || size == 0 {
            return (code::EINVAL, host.addr());
        }
        if w.registered.contains_key(&host.addr()) {
            w.last_error = 712; // CUDA_ERROR_HOST_MEMORY_ALREADY_REGISTERED
            return (code::EDRIVER, host.addr());
        }
        w.registered.insert(host.addr(), size);
        (code::OK, host.addr())
    })
}

unsafe extern "C" fn host_unregister(context: *mut c_void, host: *mut c_void) -> c_int {
    with(Operation::HostUnregister, |w| {
        if !w.contexts.contains_key(&id(context)) || w.registered.remove(&host.addr()).is_none() {
            w.last_error = 713; // CUDA_ERROR_HOST_MEMORY_NOT_REGISTERED
            return (code::EDRIVER, host.addr());
        }
        (code::OK, host.addr())
    })
}

unsafe extern "C" fn copy_d2h_async(
    stream: *mut c_void,
    dst: *mut c_void,
    src: *mut c_void,
    src_offset: u64,
    length: u64,
) -> c_int {
    with(Operation::CopyD2hAsync, |w| {
        if let Err(rc) = w.pair(stream, src) {
            return (rc, id(src));
        }
        let Some(from) = w.staging(id(src)) else {
            return (code::EINVAL, id(src));
        };
        if dst.is_null() || !span(from.len() as u64, src_offset, length) {
            return (code::ERANGE, id(src));
        }
        let (target, source) = (HostPtr(dst.cast()), id(src));
        let rc = w.enqueue(
            id(stream),
            &[(
                "staging source",
                Source::Staging {
                    buffer: source,
                    offset: src_offset,
                    length,
                },
            )],
            vec![source, dst.addr()],
            Box::new(move |w: &mut World| {
                let Some(from) = w.staging(source) else {
                    return code::EINVAL;
                };
                // SAFETY: the caller keeps the destination alive until completion.
                let out = unsafe { std::slice::from_raw_parts_mut(target.get(), length as usize) };
                out.copy_from_slice(&from[src_offset as usize..(src_offset + length) as usize]);
                w.written.push(Source::Host(target, length as usize));
                code::OK
            }),
        );
        (rc, id(src))
    })
}

unsafe extern "C" fn flush_rdma(context: *mut c_void, scope: u32) -> c_int {
    with(Operation::FlushRdma, |w| {
        if !w.contexts.contains_key(&id(context)) || (scope != 100 && scope != 200) {
            return (code::EINVAL, id(context));
        }
        if w.rdma_ordering >= scope {
            return (code::OK, id(context));
        }
        if !w.rdma_flush {
            return (code::EUNSUPPORTED, id(context));
        }
        w.rdma_flushes += 1;
        (code::OK, id(context))
    })
}

/// The simulation's function table.
pub(crate) fn api() -> Arc<Api> {
    Arc::new(Api {
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
        event,
        event_free,
        event_record,
        event_query,
        event_sync,
        stream_wait,
        host_alloc,
        host_free,
        host_register,
        host_unregister,
        copy_d2h_async,
        flush_rdma,
    })
}

/// Read `length` bytes at `offset` of a client allocation fd.
pub fn read_allocation(fd: &OwnedFd, offset: u64, length: usize) -> Vec<u8> {
    let file = File::from(fd.try_clone().expect("dup"));
    let mut out = vec![0; length];
    file.read_exact_at(&mut out, offset).expect("read memfd");
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    /// Freeing a buffer that queued work still uses is a violation, raised
    /// when the simulation drops unless taken first.
    fn free_before_complete_is_a_violation() {
        let sim = Simulation::new(vec![SimDevice::new(0)]);
        let _env = sim.enter();
        let api = api();
        let (mut context, mut stream, mut buffer) = (
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            std::ptr::null_mut(),
        );
        unsafe {
            assert_eq!((api.init)(), code::OK);
            assert_eq!((api.context)(0, &mut context), code::OK);
            assert_eq!((api.stream)(context, &mut stream), code::OK);
            assert_eq!((api.staging)(context, 4096, &mut buffer), code::OK);
            let bytes = [1u8; 16];
            assert_eq!(
                (api.copy_h2d)(stream, buffer, 0, bytes.as_ptr().cast(), 16),
                code::OK
            );
            assert_eq!((api.staging_free)(buffer), code::OK);
            assert_eq!((api.stream_free)(stream), code::OK);
        }
        let violations = sim.take_violations();
        assert_eq!(violations.len(), 1, "{violations:?}");
        assert!(violations[0].contains("freed while queued work uses it"));
    }

    #[test]
    #[should_panic(expected = "async-contract violations")]
    /// An unchecked violation panics when the simulation drops.
    fn unchecked_violation_panics_on_drop() {
        let sim = Simulation::new(vec![SimDevice::new(0)]);
        sim.world().violation("test".into());
        drop(sim);
    }

    #[test]
    /// A held operation waits on its own thread until admitted, one call per
    /// permit, while the rest of the simulation keeps running; releasing the
    /// hold lets every waiting call through.
    fn held_calls_wait_for_admission() {
        let sim = Simulation::new(vec![SimDevice::new(0)]);
        let _env = sim.enter();
        sim.hold(Operation::Init, true);
        let calls: Vec<_> = (0..3)
            .map(|_| {
                let sim = sim.clone();
                std::thread::spawn(move || {
                    let _env = sim.enter();
                    unsafe { (api().init)() }
                })
            })
            .collect();
        let wait = |count| {
            let deadline = std::time::Instant::now() + std::time::Duration::from_secs(60);
            while sim.waiting(Operation::Init) != count {
                assert!(
                    std::time::Instant::now() < deadline,
                    "held calls did not wait"
                );
                std::thread::yield_now();
            }
        };
        wait(3);
        // Unheld operations still run while calls wait.
        let mut context = std::ptr::null_mut();
        assert_eq!(unsafe { (api().context)(0, &mut context) }, code::OK);
        assert_eq!(sim.waiting(Operation::Init), 3);
        sim.admit(Operation::Init);
        wait(2);
        sim.hold(Operation::Init, false);
        wait(0);
        for call in calls {
            assert_eq!(call.join().unwrap(), code::OK);
        }
        // A permit left over from a released hold does not survive it.
        sim.admit(Operation::Init);
        sim.hold(Operation::Init, false);
        assert!(sim.world().permits.is_empty());
    }
}
