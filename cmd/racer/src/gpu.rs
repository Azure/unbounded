//! `gpu-read` backend: a coordinator thread and one executor thread per GPU.
//!
//! The coordinator (`racer-gpu`) loads the backend, discovers GPUs, imports
//! client allocations, registers files with cuFile and hands reads to the
//! cuFile reader threads, and allocates landing buffers. Each GPU's executor
//! (`racer-gpu-N`) runs the page jobs of requests whose allocation is on that
//! GPU. I/O threads talk to both through queues and get answers on one-shot
//! channels, so they never block on CUDA. Every GPU thread enters the
//! backend with [`racer_gpu::Gpu::enter_thread`], which is how tests run on
//! the simulated backend.
//!
//! # Executors
//!
//! An executor keeps up to [`STREAMS`] pages in flight, each on its own
//! stream (a lane), and learns that a page's work completed by polling the
//! fence recorded after it ([`racer_gpu::Fence::poll`], `cuEventQuery`). It
//! never synchronizes a stream on the success path. Fence events come from a
//! per-GPU [`racer_gpu::EventPool`]. With nothing in flight it sleeps on its
//! queue's condition variable; with fences pending it naps between polls,
//! from [`POLL_MIN`] doubling to [`POLL_MAX`], and a new page wakes it at
//! once.
//!
//! Each executor's queue is bounded: at most `queue_depth` page jobs
//! ([`QUEUE_DEPTH`] by default) may be queued or running on one GPU; more
//! are refused with [`Error::Overloaded`]. A request has at most one page
//! running at a time, so its pages land in order. [`MAX_OPEN`] bounds
//! requests.
//!
//! When a request ends without [`DirectSink::finish`] (for example the client
//! disconnected), dropping its sink queues a release behind its pages. The
//! executor drops the import once they are answered; a fence still holding
//! it keeps the mapping until the GPU is done. `racer_gpu` leaks and counts
//! anything whose release fails, so memory a GPU may still write is never
//! freed. When the helper is dropped, the executors finish and answer every
//! queued page before they exit.
//!
//! # Sealed pages
//!
//! Pages that are not verified plaintext in memory arrive sealed
//! ([`crate::read::sealed::SealedPage`]) and the GPU authenticates and decrypts
//! them. Each GPU has one staging buffer of [`SLOT_BYTES`] slots, allocated on
//! first use and capped by `RACER_GPU_STAGING_BYTES`:
//!
//! - The first [`scratch_slots`] slots are scratch pairs, one pair per
//!   stream when staging is large. A page takes a pair when it needs one:
//!   host or other-GPU ciphertext is copied into the pair's ciphertext slot,
//!   and a range that covers only part of a page is decrypted whole into the
//!   pair's plaintext slot (the tag covers all of it). That copy out to the
//!   import is queued only after the verdict passed, so a failed page never
//!   exposes scratch bytes. A page that needs a pair while none is free
//!   waits; pages behind it may pass.
//! - The other slots hold ciphertext that a page's owning worker read from a
//!   local v2 disk record with cuFile ([`GpuHelper::stage_file`]). A
//!   [`StagedCiphertext`] owns its slot and returns it on drop. When every
//!   slot is taken, the owner reads the record through host memory instead.
//!
//! Every source a decrypt reads (a staged or landed slot, or a scratch slot)
//! stays unchanged until the page is answered: the page owns the staged or
//! landed slot, the scratch pair is the page's until its last fence
//! completes, and the fence holds every buffer and host source. After a
//! failed page, the lane's stream is synchronized; if that fails, the stream
//! and its scratch pair are set aside and the GPU refuses sealed pages until
//! they synchronize.
//!
//! # RDMA landing
//!
//! When RDMA is active, each decrypting GPU may get one landing buffer
//! ([`GpuHelper::landing`]): separate GPU memory, capped by
//! `RACER_GPU_LANDING_BYTES`, exported once as a dma-buf. Disjoint slices of
//! it ([`GpuLanding::slice`]) are registered with the rails' NICs as
//! `rdma_verbs` device pools. A peer's NIC writes a `gpu-read` page's
//! ciphertext straight into it, and the GPU decrypts from there
//! ([`LandedCiphertext`]). The CPU never reads or copies those bytes.
//! The buffer is freed when the last [`GpuLanding`] referring to it drops:
//! the NIC registrations and received pages hold one, so this happens only
//! after every registration is gone. A registration `rdma_verbs` leaks on
//! purpose keeps its reference, so that memory is never freed. Buffers still
//! referenced when the helper exits are leaked on purpose for the same
//! reason.
//!
//! # Shared ciphertext across GPUs
//!
//! Staged and landed ciphertext is reference counted
//! ([`SealedSource`] holds an `Arc`), so several requests may decrypt the
//! same bytes. A request on the same GPU decrypts straight from the slot. A
//! request on another GPU first copies the ciphertext into the ciphertext slot of
//! its scratch pair: over NVLink or PCIe peer-to-peer when [`GpuHelper::peers`] allows
//! it (`RACER_GPU_PEER=auto`), otherwise, or after a failed peer copy, by
//! bouncing it through host memory. Each GPU still verifies the tag
//! itself. A failed peer copy turns that GPU pair off for the life of the
//! process. A peer copy is queued on the page's stream with no host wait. A
//! bounce copies into the lane's pinned host slot on the source GPU's
//! utility stream, and the page's stream waits on that fence
//! ([`racer_gpu::Stream::wait_for`]) before copying it in. The page holds the
//! source `Arc` until it is answered, so the source slot is never reused
//! while a copy may read it.
//!
//! At startup the coordinator probes each GPU with a zero-length decrypt. A GPU
//! whose shim has no decrypt kernels, or whose staging cap cannot hold its
//! scratch slots, gets plaintext pages only: the CPU decrypts as for a Get.

use crate::client::direct::Destination;
use crate::client::direct::DirectPage;
use crate::client::direct::DirectSink;
use crate::client::direct::GpuService;
use crate::config::GpuMode;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::VerifiedPage;
use crate::model::PAGE_BYTES;
use crate::read::sealed::SealedPage;
use crate::read::sealed::SealedSource;
use crate::read::sealed::SealedTarget;
use crate::runtime::RequestScope;
use futures::channel::oneshot;
use racer_gpu::DeviceInfo;
use std::collections::HashMap;
use std::ops::Range;
use std::os::fd::OwnedFd;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::Weak;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::sync::mpsc;
use std::task::Poll;

/// Concurrent `gpu-read` requests per process. More get 503.
pub const MAX_OPEN: usize = 256;

/// Bytes per staging slot: one sealed page (plaintext and tag), rounded up
/// to whole 4 KiB blocks so cuFile reads land block aligned.
pub const SLOT_BYTES: u64 = (PAGE_BYTES + 16).next_multiple_of(4096);

/// Streams each GPU executor keeps in flight: at most this many pages run
/// on one GPU at once, each with at most one decrypt outstanding on its
/// stream (the shim allows one decrypt per stream until its verdict is read).
const STREAMS: usize = 4;

/// Default bound on page jobs per GPU, queued or running. More get
/// [`Error::Overloaded`]. Each open request has at most one page in flight,
/// so this only trips when many requests target one GPU.
pub const QUEUE_DEPTH: usize = 128;

/// Idle CUDA events each GPU keeps for fences: enough for every stream's
/// page fence and a bounce fence, with a little slack.
const IDLE_EVENTS: usize = 2 * STREAMS + 4;

/// Shortest and longest nap between fence polls while pages are in flight.
/// The executor sleeps on its queue's condition variable instead, so a new
/// page wakes it at once; with nothing in flight it sleeps without a
/// timeout.
const POLL_MIN: std::time::Duration = std::time::Duration::from_micros(20);
const POLL_MAX: std::time::Duration = std::time::Duration::from_millis(1);

/// Scratch pairs (ciphertext and partial plaintext slots) per GPU with
/// `slots` staging slots: one per stream when staging is large, at least
/// one. The rest of the slots hold cuFile reads.
fn scratch_pairs(slots: u64) -> u64 {
    (slots / 32).clamp(1, STREAMS as u64)
}

/// Scratch slots per GPU with `slots` staging slots.
fn scratch_slots_for(slots: u64) -> u64 {
    2 * scratch_pairs(slots)
}

/// Scratch slots per GPU for a `RACER_GPU_STAGING_BYTES` cap. A GPU whose
/// cap holds fewer gets plaintext pages only.
pub fn scratch_slots(staging_bytes: u64) -> usize {
    scratch_slots_for(staging_bytes / SLOT_BYTES) as usize
}

/// Registered cuFile handles kept per process; more are reopened on demand.
const MAX_FILES: usize = 64;

/// Threads that run cuFile reads. One synchronous read keeps an NVMe drive
/// far from busy, and reads on the coordinator would stall imports and stages.
const FILE_READERS: usize = 4;

/// A shared ciphertext page the GPU stream holds until its copy completes.
struct HostCiphertext(crate::memory::CiphertextPage);

impl AsRef<[u8]> for HostCiphertext {
    fn as_ref(&self) -> &[u8] {
        self.0.bytes()
    }
}

/// Builds the GPU backend on the coordinator thread.
pub type Loader = Box<dyn FnOnce() -> racer_gpu::Result<racer_gpu::Gpu> + Send>;

/// Plaintext bytes handed to the helper. The page is immutable and its
/// storage is reference counted, so the helper reads it in place; the handle
/// goes back with the answer so the page is released on the I/O thread.
#[derive(Clone)]
struct Plaintext {
    page: VerifiedPage,
    range: Range<usize>,
}

impl AsRef<[u8]> for Plaintext {
    fn as_ref(&self) -> &[u8] {
        self.bytes()
    }
}

impl Plaintext {
    fn bytes(&self) -> &[u8] {
        &self.page.bytes()[self.range.clone()]
    }
}

/// A sealed page and the bytes of its plaintext the request wants.
struct Sealed {
    page: Box<SealedPage>,
    range: Range<u64>,
}

/// Work for one request on the GPU.
enum Work {
    /// Copy verified plaintext from host memory into the import.
    Plaintext(Plaintext),

    /// Verify and decrypt a sealed page into the import.
    Sealed(Sealed),
}

/// The staged-ciphertext slots of one GPU. Shared with every
/// [`StagedCiphertext`] so a slot comes back when its page is dropped, on any
/// thread, without a message to the helper.
struct SlotPool {
    free: Mutex<Vec<u64>>,
}

impl SlotPool {
    fn take(&self) -> Option<u64> {
        self.free.lock().ok()?.pop()
    }

    fn put(&self, slot: u64) {
        if let Ok(mut free) = self.free.lock() {
            free.push(slot);
        }
    }

    fn available(&self) -> usize {
        self.free.lock().map_or(0, |free| free.len())
    }
}

/// Ciphertext in one staging slot of one GPU, read there by cuFile. Owns the
/// slot until dropped. Only GPU threads touch the bytes.
pub struct StagedCiphertext {
    pool: Arc<SlotPool>,
    ordinal: i32,
    slot: u64,
}

impl StagedCiphertext {
    /// The GPU the bytes are on.
    pub fn ordinal(&self) -> i32 {
        self.ordinal
    }

    /// Byte offset of the slot in the GPU's staging buffer.
    fn offset(&self) -> u64 {
        self.slot * SLOT_BYTES
    }
}

impl Drop for StagedCiphertext {
    fn drop(&mut self) {
        self.pool.put(self.slot);
    }
}

/// Each GPU's landing buffer gets its own NIC address range from here. Any
/// value works: the address is local to the memory registration.
const LANDING_IOVA: u64 = 1 << 40;

/// One GPU landing allocation. Dropping the last reference asks the helper
/// to free the GPU memory.
struct LandingAllocation {
    /// Helper-side key of the memory.
    id: u64,
    ordinal: i32,
    /// Export of the whole allocation. `None` only during drop.
    fd: Option<OwnedFd>,
    jobs: mpsc::Sender<Job>,
}

impl Drop for LandingAllocation {
    fn drop(&mut self) {
        if let Some(fd) = self.fd.take() {
            // The helper closes the export before it frees the memory. If
            // it is gone, the memory is leaked with it.
            let _ = self.jobs.send(Job::FreeLanding { id: self.id, fd });
        }
    }
}

/// A slice of one GPU's RDMA landing buffer, shared with `rdma_verbs` as a
/// dma-buf. See the module docs, "RDMA landing".
pub struct GpuLanding {
    allocation: Arc<LandingAllocation>,
    /// Start of the slice in the allocation.
    offset: u64,
    length: u64,
}

impl GpuLanding {
    /// The GPU the memory is on.
    pub fn ordinal(&self) -> i32 {
        self.allocation.ordinal
    }

    /// `length` bytes at `offset` of this slice, as their own slice that
    /// keeps the GPU memory alive. `None` unless both are 4 KiB aligned,
    /// `length` is nonzero, and the range fits.
    pub fn slice(&self, offset: u64, length: u64) -> Option<Arc<GpuLanding>> {
        let aligned = offset.is_multiple_of(4096) && length.is_multiple_of(4096);
        (aligned && length != 0 && offset.checked_add(length)? <= self.length).then(|| {
            Arc::new(GpuLanding {
                allocation: self.allocation.clone(),
                offset: self.offset + offset,
                length,
            })
        })
    }

    /// True if `other` is a slice of the same GPU memory.
    pub fn same_allocation(&self, other: &GpuLanding) -> bool {
        Arc::ptr_eq(&self.allocation, &other.allocation)
    }
}

impl rdma_verbs::DmabufMemory for GpuLanding {
    fn fd(&self) -> std::os::fd::BorrowedFd<'_> {
        use std::os::fd::AsFd;
        self.allocation
            .fd
            .as_ref()
            .expect("landing export lives until drop")
            .as_fd()
    }

    fn offset(&self) -> u64 {
        self.offset
    }

    fn length(&self) -> usize {
        self.length as usize
    }

    fn iova(&self) -> u64 {
        LANDING_IOVA * (self.allocation.ordinal as u64 + 1) + self.offset
    }
}

/// Peer ciphertext that a NIC wrote into a GPU landing buffer. Holds the
/// RDMA device page, so the slot is not reused until this is dropped.
pub struct LandedCiphertext {
    /// Holds the slot. `None` only for test pages written without a NIC.
    _page: Option<rdma_verbs::DevicePage>,
    ordinal: i32,
    /// Helper-side key of the landing allocation.
    allocation: u64,
    /// Byte offset in the allocation.
    offset: u64,
    length: u64,
}

impl LandedCiphertext {
    /// Wrap `page` if it lies in `landing`'s memory.
    pub fn new(landing: &Arc<GpuLanding>, page: rdma_verbs::DevicePage) -> Result<Self> {
        let memory = Arc::as_ptr(page.memory()).cast::<()>();
        if !std::ptr::eq(memory, Arc::as_ptr(landing).cast::<()>()) {
            return Err(Error::Internal);
        }
        Ok(Self {
            ordinal: landing.ordinal(),
            allocation: landing.allocation.id,
            offset: landing.offset + page.offset(),
            length: page.len() as u64,
            _page: Some(page),
        })
    }

    /// Bytes at `offset` of `landing`, written there by a test in place of
    /// a NIC.
    #[cfg(test)]
    pub(crate) fn for_test(landing: &GpuLanding, offset: u64, length: u64) -> Self {
        assert!(offset + length <= landing.length);
        Self {
            _page: None,
            ordinal: landing.ordinal(),
            allocation: landing.allocation.id,
            offset: landing.offset + offset,
            length,
        }
    }

    /// The GPU the bytes are on.
    pub fn ordinal(&self) -> i32 {
        self.ordinal
    }

    /// Bytes that landed.
    pub fn len(&self) -> u64 {
        self.length
    }

    /// Never true: landed ciphertext always holds a tag.
    pub fn is_empty(&self) -> bool {
        self.length == 0
    }

    /// Byte offset in the landing allocation.
    pub(crate) fn offset(&self) -> u64 {
        self.offset
    }
}

/// Counters for GPU decryption, for logs and tests.
#[derive(Debug, Default)]
pub struct GpuCounters {
    /// Sealed pages the GPU verified and wrote.
    pub decrypted: AtomicU64,

    /// Sealed pages the GPU rejected (tag mismatch).
    pub rejected: AtomicU64,

    /// Pages cuFile read straight into staging.
    pub staged: AtomicU64,

    /// Direct reads that found no free staging slot.
    pub exhausted: AtomicU64,

    /// cuFile reads or registrations that failed.
    pub file_failures: AtomicU64,

    /// Sealed pages decrypted from an RDMA landing buffer.
    pub landed: AtomicU64,

    /// Verified plaintext pages copied from host memory (decrypted by the
    /// CPU or already plaintext in cache).
    pub plaintext: AtomicU64,

    /// Devices (`st_dev`) cuFile rejected as unsupported. Each device is
    /// counted once; cuFile stays on for other devices.
    pub cufile_unsupported: AtomicU64,

    /// Times cuFile was turned off, indexed by [`CufileDisable`].
    pub cufile_disabled: [AtomicU64; CufileDisable::ALL.len()],

    /// Sealed pages whose ciphertext was copied from another GPU over a
    /// peer path (NVLink or PCIe P2P).
    pub peer_copies: AtomicU64,

    /// Ciphertext bytes copied between GPUs over a peer path.
    pub peer_copy_bytes: AtomicU64,

    /// Peer copies that failed. Each failure turns its GPU pair off.
    pub peer_failures: AtomicU64,

    /// Sealed pages whose ciphertext moved from another GPU through host
    /// memory, because the pair has no peer path or it was turned off.
    pub peer_bounces: AtomicU64,

    /// Sealed fetches that reused GPU ciphertext another request already
    /// had, instead of fetching it again. See [`GpuHelper::record_joined`].
    pub joined: AtomicU64,

    /// Pages refused because their GPU's executor queue was full.
    pub queue_full: AtomicU64,
}

/// Why cuFile was turned off.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CufileDisable {
    /// The cuFile driver failed to open at startup.
    DriverOpen,
    /// A staging buffer could not be registered with cuFile.
    StagingRegister,
    /// A file could not be registered with cuFile.
    FileOpen,
}

impl CufileDisable {
    pub const ALL: [Self; 3] = [Self::DriverOpen, Self::StagingRegister, Self::FileOpen];

    /// Metric label value.
    pub fn label(self) -> &'static str {
        match self {
            Self::DriverOpen => "driver_open",
            Self::StagingRegister => "staging_register",
            Self::FileOpen => "file_open",
        }
    }
}

impl GpuCounters {
    /// Count one cuFile turn-off.
    pub(crate) fn cufile_disabled(&self, reason: CufileDisable) {
        self.cufile_disabled[reason as usize].fetch_add(1, Ordering::Relaxed);
    }
}

/// Which GPU pairs may copy from each other over a peer path. Indexed by
/// position in the usable GPU list, because ordinals need not be dense.
/// Written once by the coordinator at startup; entries only turn off after.
struct PeerMatrix {
    ordinals: Vec<i32>,
    /// `allowed[dst * n + src]`.
    allowed: Vec<AtomicBool>,
}

impl PeerMatrix {
    fn entry(&self, dst: i32, src: i32) -> Option<&AtomicBool> {
        let position = |ordinal| self.ordinals.iter().position(|o| *o == ordinal);
        let (dst, src) = (position(dst)?, position(src)?);
        self.allowed.get(dst * self.ordinals.len() + src)
    }
}

/// Startup options for [`GpuHelper`] beyond the mode.
#[derive(Clone, Copy, Debug)]
pub struct GpuOptions {
    /// Per-GPU staging cap (`RACER_GPU_STAGING_BYTES`).
    pub staging_bytes: u64,
    /// Ask cuFile to use PCI P2PDMA (`RACER_GPU_PCI_P2PDMA`).
    pub pci_p2pdma: bool,
    /// Enable GPU peer access (`RACER_GPU_PEER=auto`).
    pub peer: bool,
    /// Page jobs each GPU executor accepts, queued or running, before
    /// [`Error::Overloaded`]. See [`QUEUE_DEPTH`].
    pub queue_depth: usize,
}

impl Default for GpuOptions {
    fn default() -> Self {
        Self {
            staging_bytes: crate::config::DEFAULT_GPU_STAGING_BYTES,
            pci_p2pdma: false,
            peer: true,
            queue_depth: QUEUE_DEPTH,
        }
    }
}

/// Per-GPU state shared with I/O threads.
struct DeviceShared {
    ordinal: i32,

    /// The startup decrypt probe passed and staging holds the scratch slots.
    decrypt: bool,

    slots: Arc<SlotPool>,

    /// The GPU's executor queue.
    queue: Arc<Queue>,
}

/// A message to the coordinator thread.
enum Job {
    Import {
        id: u64,
        fd: OwnedFd,
        allocation_size: u64,
        window: Range<u64>,
        /// The client's `Racer-Gpu-Uuid`, when it sent one.
        gpu_uuid: Option<[u8; 16]>,
        reply: oneshot::Sender<Result<i32>>,
    },
    /// Read ciphertext from a file straight into a staging slot.
    Stage {
        staged: StagedCiphertext,
        fd: OwnedFd,
        file_offset: u64,
        length: u64,
        reply: oneshot::Sender<Result<StagedCiphertext>>,
    },
    /// Release a request whose GPU was not known when it ended. Forwarded
    /// to that GPU's executor once the import has answered.
    Release {
        id: u64,
        reply: Option<oneshot::Sender<Result<()>>>,
    },
    /// Allocate a GPU's RDMA landing buffer and export it as a dma-buf.
    Landing {
        id: u64,
        ordinal: i32,
        length: u64,
        reply: oneshot::Sender<Result<OwnedFd>>,
    },
    /// Close a landing buffer's export, then free it: nothing refers to it.
    FreeLanding { id: u64, fd: OwnedFd },
    #[cfg(test)]
    Run(Box<dyn FnOnce() + Send>),
}

/// One page for a request, run by its GPU's executor.
struct WriteTask {
    id: u64,
    offset: u64,
    work: Work,
    reply: oneshot::Sender<(Work, Result<()>)>,
}

/// A message to a GPU executor.
enum Task {
    /// Request `id` imported its allocation on this GPU.
    Adopt {
        id: u64,
        import: Arc<racer_gpu::ImportedAllocation>,
    },
    Write(WriteTask),
    /// Release request `id` once its queued and running pages are done.
    Release {
        id: u64,
        reply: Option<oneshot::Sender<Result<()>>>,
    },
}

/// A GPU executor's inbox. Page jobs are bounded by `depth`, counted from
/// submission until their answer; control messages are not, because each
/// request sends at most two. The executor sleeps on the condition variable
/// while it has nothing in flight.
struct Queue {
    state: Mutex<QueueState>,
    ready: std::sync::Condvar,
    depth: usize,
    /// Page jobs submitted and not yet answered.
    pending: AtomicUsize,
}

#[derive(Default)]
struct QueueState {
    tasks: std::collections::VecDeque<Task>,
    closed: bool,
}

impl Queue {
    fn new(depth: usize) -> Self {
        Self {
            state: Mutex::new(QueueState::default()),
            ready: std::sync::Condvar::new(),
            depth,
            pending: AtomicUsize::new(0),
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, QueueState> {
        self.state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// Queue a control message. [`Error::Unavailable`] once closed.
    fn control(&self, task: Task) -> Result<()> {
        let mut state = self.lock();
        if state.closed {
            return Err(Error::Unavailable);
        }
        state.tasks.push_back(task);
        self.ready.notify_one();
        Ok(())
    }

    /// Queue a page job. [`Error::Overloaded`] when `depth` jobs are already
    /// queued or running, [`Error::Unavailable`] once closed.
    fn write(&self, task: WriteTask) -> Result<()> {
        let mut state = self.lock();
        if state.closed {
            return Err(Error::Unavailable);
        }
        if self.pending.load(Ordering::Acquire) >= self.depth {
            return Err(Error::Overloaded);
        }
        self.pending.fetch_add(1, Ordering::AcqRel);
        state.tasks.push_back(Task::Write(task));
        self.ready.notify_one();
        Ok(())
    }

    /// A page job was answered.
    fn finished(&self) {
        self.pending.fetch_sub(1, Ordering::AcqRel);
    }

    /// Take every queued message. With nothing queued, wait for one: up to
    /// `wait`, or until one arrives or the queue closes when `wait` is
    /// `None`. Also returns whether the queue is closed.
    fn take(&self, wait: Option<std::time::Duration>) -> (Vec<Task>, bool) {
        let mut state = self.lock();
        if state.tasks.is_empty() && !state.closed {
            state = match wait {
                None => self
                    .ready
                    .wait_while(state, |state| state.tasks.is_empty() && !state.closed)
                    .unwrap_or_else(|poisoned| poisoned.into_inner()),
                Some(wait) => {
                    self.ready
                        .wait_timeout(state, wait)
                        .unwrap_or_else(|poisoned| poisoned.into_inner())
                        .0
                }
            };
        }
        let closed = state.closed;
        (state.tasks.drain(..).collect(), closed)
    }

    /// Refuse new messages; the executor drains what is queued and exits.
    fn close(&self) {
        self.lock().closed = true;
        self.ready.notify_all();
    }
}

/// Handle to the GPU threads. Shared by every worker.
pub struct GpuHelper {
    jobs: mpsc::Sender<Job>,
    devices: Vec<DeviceInfo>,
    next: AtomicU64,
    open: AtomicUsize,
    leaked: Arc<AtomicU64>,
    shared: Arc<Vec<DeviceShared>>,
    /// cuFile is worth trying: no driver-level failure has been seen.
    cufile: Arc<AtomicBool>,
    counters: Arc<GpuCounters>,
    kernels_off: Arc<Mutex<std::collections::HashSet<i32>>>,
    /// Live landing buffers, one per GPU at most. Weak: a buffer is freed
    /// once nothing registers or reads it, and made again on demand.
    landings: Mutex<HashMap<i32, Weak<GpuLanding>>>,
    /// GPU pairs with a working peer path. Set before startup answers.
    peers: Arc<std::sync::OnceLock<PeerMatrix>>,
    /// The GPU each imported request is on, until its executor releases it.
    owners: Arc<Mutex<HashMap<u64, i32>>>,
    /// Streams the executors hold between requests.
    streams: Arc<AtomicUsize>,
}

impl GpuHelper {
    /// Resolve `mode` against hardware. Returns `None` when GPU mode is off.
    pub fn start(mode: GpuMode, options: GpuOptions) -> Result<Option<Arc<Self>>> {
        Self::start_built(
            mode,
            cfg!(feature = "cuda"),
            options,
            Box::new(racer_gpu::Gpu::load),
        )
    }

    /// [`Self::start`] with `built` standing in for the `cuda` feature and a
    /// custom backend loader. `loader` is not called when GPU mode is off.
    pub fn start_built(
        mode: GpuMode,
        built: bool,
        options: GpuOptions,
        loader: Loader,
    ) -> Result<Option<Arc<Self>>> {
        match mode {
            GpuMode::Disabled => Ok(None),
            GpuMode::Auto if !built => {
                eprintln!("racer: stage=gpu state=disabled reason=not-built");
                Ok(None)
            }
            _ => Self::start_with_options(mode, options, loader),
        }
    }

    /// Start the helper with a custom backend loader and the default staging cap.
    pub fn start_with(mode: GpuMode, loader: Loader) -> Result<Option<Arc<Self>>> {
        Self::start_with_staging(mode, crate::config::DEFAULT_GPU_STAGING_BYTES, loader)
    }

    /// Start the helper with a custom backend loader and a per-GPU staging cap.
    pub fn start_with_staging(
        mode: GpuMode,
        staging_bytes: u64,
        loader: Loader,
    ) -> Result<Option<Arc<Self>>> {
        let options = GpuOptions {
            staging_bytes,
            ..GpuOptions::default()
        };
        Self::start_with_options(mode, options, loader)
    }

    /// Start the helper with a custom backend loader and options.
    pub fn start_with_options(
        mode: GpuMode,
        options: GpuOptions,
        loader: Loader,
    ) -> Result<Option<Arc<Self>>> {
        let staging_bytes = options.staging_bytes;
        let pci_p2pdma = options.pci_p2pdma;
        if mode == GpuMode::Disabled {
            return Ok(None);
        }
        let (jobs, receiver) = mpsc::channel();
        let (ready, started) = mpsc::sync_channel(1);
        let leaked = Arc::new(AtomicU64::new(0));
        let cufile = Arc::new(AtomicBool::new(true));
        let counters = Arc::new(GpuCounters::default());
        let slots = staging_bytes / SLOT_BYTES;
        let kernels_off = Arc::new(Mutex::new(std::collections::HashSet::new()));
        let peers = Arc::new(std::sync::OnceLock::new());
        let owners = Arc::new(Mutex::new(HashMap::new()));
        let streams = Arc::new(AtomicUsize::new(0));
        let state = HelperState {
            peer: options.peer,
            peers: peers.clone(),
            leaked: leaked.clone(),
            cufile: cufile.clone(),
            counters: counters.clone(),
            kernels_off: kernels_off.clone(),
            slots,
            pci_p2pdma,
            queue_depth: options.queue_depth.max(1),
            owners: owners.clone(),
            streams: streams.clone(),
        };
        std::thread::Builder::new()
            .name("racer-gpu".into())
            .spawn(move || coordinator_main(loader, receiver, ready, state))
            .map_err(|_| Error::Unavailable)?;
        let found = started.recv().map_err(|_| Error::Unavailable)?;
        let (devices, decrypt, queues) = match found {
            Ok((devices, decrypt, queues)) if !devices.is_empty() => (devices, decrypt, queues),
            outcome => {
                let reason = match outcome {
                    Err(error) => error.to_string(),
                    Ok(_) => "no-usable-gpu".into(),
                };
                eprintln!("racer: stage=gpu state=disabled reason={reason}");
                return if mode == GpuMode::Required {
                    Err(Error::Unavailable)
                } else {
                    Ok(None)
                };
            }
        };
        let scratch = scratch_slots(staging_bytes) as u64;
        let decrypting = slots >= scratch;
        let shared = devices
            .iter()
            .zip(decrypt.iter().zip(queues))
            .map(|(device, (kernels, queue))| DeviceShared {
                ordinal: device.ordinal,
                decrypt: *kernels && decrypting,
                slots: Arc::new(SlotPool {
                    free: Mutex::new((scratch..slots.max(scratch)).rev().collect()),
                }),
                queue,
            })
            .collect::<Vec<_>>();
        eprintln!(
            "racer: stage=gpu state=enabled usable={} decrypt={} staging_slots={} scratch_slots={} streams={} queue_depth={} pci_p2pdma={}",
            devices.len(),
            shared.iter().filter(|device| device.decrypt).count(),
            slots,
            scratch,
            STREAMS,
            options.queue_depth.max(1),
            pci_p2pdma,
        );
        Ok(Some(Arc::new(Self {
            jobs,
            devices,
            next: AtomicU64::new(1),
            open: AtomicUsize::new(0),
            leaked,
            shared: Arc::new(shared),
            cufile,
            counters,
            kernels_off,
            landings: Mutex::new(HashMap::new()),
            peers,
            owners,
            streams,
        })))
    }

    /// Whether GPU `dst` may copy ciphertext from GPU `src` over a peer
    /// path. True when they are the same GPU. False when
    /// `RACER_GPU_PEER=off`, when the pair has no peer path, or after a peer
    /// copy between them failed. Ciphertext on another GPU still works when
    /// this is false: the executor moves it through host memory.
    pub fn peers(&self, dst: i32, src: i32) -> bool {
        dst == src
            || self
                .peers
                .get()
                .and_then(|matrix| matrix.entry(dst, src))
                .is_some_and(|allowed| allowed.load(Ordering::Relaxed))
    }

    /// Count a sealed fetch that reused GPU ciphertext another request
    /// already had instead of fetching it.
    pub fn record_joined(&self) {
        self.counters.joined.fetch_add(1, Ordering::Relaxed);
    }

    /// GPU decryption counters.
    pub fn counters(&self) -> &GpuCounters {
        &self.counters
    }

    fn device(&self, ordinal: i32) -> Option<&DeviceShared> {
        self.shared.iter().find(|device| device.ordinal == ordinal)
    }

    /// Whether sealed pages can be decrypted on this GPU.
    pub fn decrypts(&self, ordinal: i32) -> bool {
        self.device(ordinal).is_some_and(|device| device.decrypt)
            && self
                .kernels_off
                .lock()
                .is_ok_and(|off| !off.contains(&ordinal))
    }

    /// Free staged-ciphertext slots on this GPU.
    pub fn free_slots(&self, ordinal: i32) -> usize {
        self.device(ordinal)
            .map_or(0, |device| device.slots.available())
    }

    /// Page jobs queued or running on GPU `ordinal`'s executor.
    pub fn queued(&self, ordinal: i32) -> usize {
        self.device(ordinal)
            .map_or(0, |device| device.queue.pending.load(Ordering::Relaxed))
    }

    /// Streams the executors hold now: their request streams, the utility
    /// streams used to bounce ciphertext, and streams kept after a failed
    /// fence. None of them belongs to a request.
    pub fn executor_streams(&self) -> usize {
        self.streams.load(Ordering::Relaxed)
    }

    /// Whether a cuFile read is worth trying.
    pub fn cufile(&self) -> bool {
        self.cufile.load(Ordering::Relaxed)
    }

    /// Read `length` bytes at `file_offset` of `fd` into a staging slot on
    /// GPU `ordinal` with cuFile. Returns [`Error::Overloaded`] when every
    /// slot is taken, and [`Error::Unavailable`] when cuFile cannot read the
    /// file; the caller then reads through host memory. The caller must keep
    /// the bytes from changing (for example by holding a segment lease) until
    /// this resolves.
    pub(crate) async fn stage_file(
        &self,
        ordinal: i32,
        fd: OwnedFd,
        file_offset: u64,
        length: u64,
        scope: &RequestScope,
    ) -> Result<StagedCiphertext> {
        if !self.cufile() || length == 0 || length > SLOT_BYTES {
            return Err(Error::Unavailable);
        }
        let device = self.device(ordinal).ok_or(Error::Unavailable)?;
        let Some(slot) = device.slots.take() else {
            self.counters.exhausted.fetch_add(1, Ordering::Relaxed);
            return Err(Error::Overloaded);
        };
        let staged = StagedCiphertext {
            pool: device.slots.clone(),
            ordinal,
            slot,
        };
        let (reply, mut receiver) = oneshot::channel();
        self.send(Job::Stage {
            staged,
            fd,
            file_offset,
            length,
            reply,
        })?;
        // On cancellation the read still runs; the staged slot then drops
        // with the unread answer and returns to the pool.
        answer(&mut receiver, scope).await?
    }

    /// GPU `ordinal`'s RDMA landing buffer of `length` bytes (a multiple of
    /// 4 KiB), allocated and exported on first call and reused after that.
    /// [`Error::Unavailable`] if the GPU does not decrypt or the export
    /// fails, [`Error::Overloaded`] if the GPU has no room.
    pub(crate) async fn landing(
        &self,
        ordinal: i32,
        length: u64,
        scope: &RequestScope,
    ) -> Result<Arc<GpuLanding>> {
        if !self.decrypts(ordinal) || length == 0 || !length.is_multiple_of(4096) {
            return Err(Error::Unavailable);
        }
        if let Some(landing) = self
            .landings
            .lock()
            .ok()
            .and_then(|l| l.get(&ordinal).and_then(Weak::upgrade))
        {
            return Ok(landing);
        }
        let id = self.next.fetch_add(1, Ordering::Relaxed);
        let (reply, mut receiver) = oneshot::channel();
        self.send(Job::Landing {
            id,
            ordinal,
            length,
            reply,
        })?;
        // Dropped on any failure below, which frees the memory again.
        let made = Arc::new(GpuLanding {
            allocation: Arc::new(LandingAllocation {
                id,
                ordinal,
                fd: Some(answer(&mut receiver, scope).await??),
                jobs: self.jobs.clone(),
            }),
            offset: 0,
            length,
        });
        let mut landings = self.landings.lock().map_err(|_| Error::Internal)?;
        // A concurrent caller may have made one first: keep that one.
        if let Some(landing) = landings.get(&ordinal).and_then(Weak::upgrade) {
            return Ok(landing);
        }
        landings.insert(ordinal, Arc::downgrade(&made));
        Ok(made)
    }

    /// Where a request on GPU `ordinal` should have sealed pages delivered,
    /// or None to get plaintext only.
    pub fn sealed_target(self: &Arc<Self>, ordinal: i32) -> Option<SealedTarget> {
        self.decrypts(ordinal).then(|| SealedTarget {
            helper: self.clone(),
            ordinal,
        })
    }

    /// Usable GPUs found at startup.
    pub fn devices(&self) -> &[DeviceInfo] {
        &self.devices
    }

    /// GPU resources leaked on purpose after a failed release.
    pub fn leaked(&self) -> u64 {
        self.leaked.load(Ordering::Relaxed)
    }

    /// Requests that currently hold a sink.
    pub fn open_requests(&self) -> usize {
        self.open.load(Ordering::Relaxed)
    }

    fn send(&self, job: Job) -> Result<()> {
        self.jobs.send(job).map_err(|_| Error::Unavailable)
    }

    /// Queue a page for request `id` on GPU `ordinal`'s executor.
    fn submit(&self, ordinal: i32, task: WriteTask) -> Result<()> {
        let device = self.device(ordinal).ok_or(Error::Internal)?;
        let result = device.queue.write(task);
        if result == Err(Error::Overloaded) {
            self.counters.queue_full.fetch_add(1, Ordering::Relaxed);
        }
        result
    }

    /// Release request `id`: on its executor, behind its pages, or through
    /// the coordinator when its import has not answered yet.
    fn release(&self, id: u64, reply: Option<oneshot::Sender<Result<()>>>) -> Result<()> {
        let owner = self
            .owners
            .lock()
            .ok()
            .and_then(|owners| owners.get(&id).copied());
        match owner.and_then(|ordinal| self.device(ordinal)) {
            Some(device) => device.queue.control(Task::Release { id, reply }),
            None => self.send(Job::Release { id, reply }),
        }
    }

    /// Run `f` on the coordinator thread and wait for it. Tests use this to
    /// reach thread-local simulation state.
    #[cfg(test)]
    pub(crate) fn run<R: Send + 'static>(&self, f: impl FnOnce() -> R + Send + 'static) -> R {
        let (tx, rx) = mpsc::channel();
        self.send(Job::Run(Box::new(move || {
            let _ = tx.send(f());
        })))
        .unwrap();
        rx.recv().unwrap()
    }
}

/// The per-worker [`GpuService`].
pub struct GpuClient {
    helper: Arc<GpuHelper>,
}

impl GpuClient {
    pub fn new(helper: Arc<GpuHelper>) -> Self {
        Self { helper }
    }
}

impl GpuService for GpuClient {
    fn open(
        &self,
        descriptor: OwnedFd,
        destination: Destination,
        allocation_size: u64,
        gpu_uuid: Option<[u8; 16]>,
    ) -> Result<Box<dyn DirectSink>> {
        let window = destination.offset..destination.end();
        if window.start >= window.end || window.end > allocation_size {
            return Err(Error::InvalidRequest);
        }
        let open = &self.helper.open;
        if open.fetch_add(1, Ordering::AcqRel) >= MAX_OPEN {
            open.fetch_sub(1, Ordering::AcqRel);
            return Err(Error::Overloaded);
        }
        let mut sink = GpuSink {
            helper: self.helper.clone(),
            id: self.helper.next.fetch_add(1, Ordering::Relaxed),
            window: window.clone(),
            import: None,
            ordinal: None,
            released: false,
        };
        let (reply, answer) = oneshot::channel();
        sink.helper.send(Job::Import {
            id: sink.id,
            fd: descriptor,
            allocation_size,
            window,
            gpu_uuid,
            reply,
        })?;
        sink.import = Some(answer);
        Ok(Box::new(sink))
    }
}

/// One `gpu-read` request's view of its import.
struct GpuSink {
    helper: Arc<GpuHelper>,
    id: u64,
    window: Range<u64>,
    /// The pending import answer, until it has been checked.
    import: Option<oneshot::Receiver<Result<i32>>>,
    /// The GPU that holds the allocation, once imported.
    ordinal: Option<i32>,
    released: bool,
}

/// Wait for an answer, or stop early when the request is canceled.
/// Abandoning the wait never abandons the job: it still runs.
async fn answer<T>(receiver: &mut oneshot::Receiver<T>, scope: &RequestScope) -> Result<T> {
    let registration = scope.cancellation.subscribe()?;
    std::future::poll_fn(|cx| {
        registration.register(cx.waker());
        if let Poll::Ready(result) = std::pin::Pin::new(&mut *receiver).poll(cx) {
            return Poll::Ready(result.map_err(|_| Error::Unavailable));
        }
        scope.check()?;
        Poll::Pending
    })
    .await
}

use std::future::Future;

impl GpuSink {
    async fn imported(&mut self, scope: &RequestScope) -> Result<()> {
        if let Some(receiver) = self.import.as_mut() {
            let result = answer(receiver, scope).await?;
            self.import = None;
            self.ordinal = Some(result?);
        }
        Ok(())
    }
}

impl DirectSink for GpuSink {
    fn ready<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(self.imported(scope))
    }

    fn deliver<'a>(
        &'a mut self,
        page: DirectPage<'a>,
        destination_offset: u64,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            let end = destination_offset
                .checked_add(page.len())
                .ok_or(Error::InvalidRange)?;
            if destination_offset < self.window.start || end > self.window.end {
                return Err(Error::InvalidRange);
            }
            self.imported(scope).await?;
            let ordinal = self.ordinal.ok_or(Error::Internal)?;
            let work = match page {
                DirectPage::Plaintext(reader) => {
                    let slice = reader.slice();
                    let start = slice.offset as usize;
                    Work::Plaintext(Plaintext {
                        page: reader.page().clone(),
                        range: start..start + slice.length as usize,
                    })
                }
                DirectPage::Sealed { page, range } => Work::Sealed(Sealed { page, range }),
            };
            let (reply, mut receiver) = oneshot::channel();
            self.helper.submit(
                ordinal,
                WriteTask {
                    id: self.id,
                    offset: destination_offset,
                    work,
                    reply,
                },
            )?;
            let (work, result) = answer(&mut receiver, scope).await?;
            drop(work);
            result
        })
    }

    fn sealed_target(&self) -> Option<SealedTarget> {
        self.helper.sealed_target(self.ordinal?)
    }

    fn finish<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            if self.released {
                return Ok(());
            }
            let (reply, mut receiver) = oneshot::channel();
            self.helper.release(self.id, Some(reply))?;
            self.released = true;
            answer(&mut receiver, scope).await?
        })
    }
}

impl Drop for GpuSink {
    fn drop(&mut self) {
        if !self.released {
            // Queued behind any page still running on the executor.
            let _ = self.helper.release(self.id, None);
        }
        self.helper.open.fetch_sub(1, Ordering::AcqRel);
    }
}

/// Ciphertext on a GPU other than the request's.
struct Remote {
    ordinal: i32,
    /// The landing allocation holding it, or `None` for the staging buffer.
    landed: Option<u64>,
    offset: u64,
}

/// Shared handles the GPU threads start with.
struct HelperState {
    /// `RACER_GPU_PEER=auto`: try to enable peer access between GPUs.
    peer: bool,
    peers: Arc<std::sync::OnceLock<PeerMatrix>>,
    leaked: Arc<AtomicU64>,
    cufile: Arc<AtomicBool>,
    counters: Arc<GpuCounters>,
    /// GPUs whose decrypt kernels failed after startup.
    kernels_off: Arc<Mutex<std::collections::HashSet<i32>>>,
    /// Staging slots per GPU.
    slots: u64,
    /// Ask cuFile to use PCI P2PDMA.
    pci_p2pdma: bool,
    /// Page jobs per executor queue.
    queue_depth: usize,
    owners: Arc<Mutex<HashMap<u64, i32>>>,
    streams: Arc<AtomicUsize>,
}

/// A registered cuFile handle and the reads using it.
struct OpenFile {
    file: racer_gpu::GpuFile,
    /// Reads queued or running on a reader thread.
    reads: usize,
    /// No longer in [`Coordinator::files`]: free it once `reads` is zero.
    retired: bool,
}

/// A cuFile read for a reader thread.
struct FileJob {
    read: racer_gpu::FileRead,
    file: u64,
    staged: StagedCiphertext,
    /// Keeps the staging buffer the read writes alive until it returns.
    _staging: Arc<racer_gpu::StagingBuffer>,
    reply: oneshot::Sender<Result<StagedCiphertext>>,
}

/// A finished cuFile read.
struct FileDone {
    file: u64,
    failed: bool,
}

/// Run cuFile reads until the coordinator stops sending them. Reports each
/// read before answering, so a later stage sees a failed handle gone.
fn reader_main(
    reader: racer_gpu::FileReader,
    jobs: Arc<Mutex<mpsc::Receiver<FileJob>>>,
    done: mpsc::Sender<FileDone>,
    counters: Arc<GpuCounters>,
) {
    loop {
        let job = match jobs.lock() {
            Ok(jobs) => jobs.recv(),
            Err(_) => return,
        };
        let Ok(job) = job else {
            return;
        };
        let result = reader.read(job.read);
        let failed = result.is_err();
        if let Err(error) = &result {
            counters.file_failures.fetch_add(1, Ordering::Relaxed);
            eprintln!("racer: stage=gpu state=cufile-read-failed reason={error}");
        } else {
            counters.staged.fetch_add(1, Ordering::Relaxed);
        }
        let _ = done.send(FileDone {
            file: job.file,
            failed,
        });
        // A dropped receiver drops the slot, which frees it.
        let _ = job
            .reply
            .send(result.map(|()| job.staged).map_err(|_| Error::Unavailable));
    }
}

/// One GPU, shared by the coordinator and every executor.
struct Device {
    ordinal: i32,
    context: racer_gpu::Context,
    /// The staging buffer, allocated on first use.
    staging: Mutex<Option<Arc<racer_gpu::StagingBuffer>>>,
    /// A stream for reading ciphertext back to the host when it moves to a
    /// GPU without a peer path. Made on first use; any executor may queue
    /// on it.
    utility: Mutex<Option<Arc<racer_gpu::Stream>>>,
    /// Events for fences on this GPU's streams.
    events: racer_gpu::EventPool,
    queue: Arc<Queue>,
}

/// GPU state the coordinator and the executors share.
struct Engine {
    gpu: racer_gpu::Gpu,
    devices: Vec<Arc<Device>>,
    /// RDMA landing buffers by allocation id. Freed by `FreeLanding` once
    /// nothing refers to them; leaked if still referenced at exit, because
    /// a NIC may hold a registration over them.
    landing: Mutex<HashMap<u64, Arc<racer_gpu::StagingBuffer>>>,
    state: HelperState,
}

impl Engine {
    fn device(&self, ordinal: i32) -> Result<&Arc<Device>> {
        self.devices
            .iter()
            .find(|device| device.ordinal == ordinal)
            .ok_or(Error::Internal)
    }

    /// GPU `ordinal`'s staging buffer, allocated on first use.
    fn staging(&self, ordinal: i32) -> Result<Arc<racer_gpu::StagingBuffer>> {
        let device = self.device(ordinal)?;
        let mut staging = device.staging.lock().map_err(|_| Error::Internal)?;
        if let Some(buffer) = staging.as_ref() {
            return Ok(buffer.clone());
        }
        let size = self.state.slots.max(scratch_slots_for(self.state.slots)) * SLOT_BYTES;
        let buffer = device.context.staging(size).map_err(|error| {
            eprintln!("racer: stage=gpu ordinal={ordinal} state=staging-failed reason={error}");
            Error::Overloaded
        })?;
        let buffer = Arc::new(buffer);
        *staging = Some(buffer.clone());
        Ok(buffer)
    }

    /// GPU `ordinal`'s utility stream, made on first use.
    fn utility(&self, ordinal: i32) -> Result<Arc<racer_gpu::Stream>> {
        let device = self.device(ordinal)?;
        let mut utility = device.utility.lock().map_err(|_| Error::Internal)?;
        if let Some(stream) = utility.as_ref() {
            return Ok(stream.clone());
        }
        let stream = Arc::new(device.context.stream().map_err(gpu_error)?);
        self.state.streams.fetch_add(1, Ordering::Relaxed);
        *utility = Some(stream.clone());
        Ok(stream)
    }

    /// The buffer that holds ciphertext on another GPU.
    fn source_buffer(&self, source: &Remote) -> Result<Arc<racer_gpu::StagingBuffer>> {
        match source.landed {
            Some(allocation) => self
                .landing
                .lock()
                .map_err(|_| Error::Internal)?
                .get(&allocation)
                .cloned()
                .ok_or(Error::Internal),
            None => self.staging(source.ordinal),
        }
    }

    /// Make a NIC's writes into landed ciphertext on GPU `owner` visible
    /// before the GPU (or, for `remote`, another GPU's peer copy) reads it.
    fn flush_landed(&self, owner: i32, remote: bool) -> Result<()> {
        let scope = if remote {
            racer_gpu::RdmaScope::AllDevices
        } else {
            racer_gpu::RdmaScope::Owner
        };
        self.device(owner)?
            .context
            .flush_rdma_writes(scope)
            .map_err(|error| {
                eprintln!(
                    "racer: stage=gpu ordinal={owner} state=rdma-flush-failed reason={error}"
                );
                Error::Unavailable
            })
    }

    /// After a failed sealed write, make sure no queued GPU work still reads
    /// the ciphertext's staged slot or landing page before the page drops
    /// and the slot goes back for another cuFile read or NIC write. The
    /// streams retain the GPU allocation, not the slot. If no fence
    /// succeeds, the slot is quarantined for the life of the process.
    /// `fenced` says whether the request stream synchronized.
    fn fence_source(&self, mut fenced: bool, source: &SealedSource) {
        let ordinal = match source {
            SealedSource::Host(_) => return,
            SealedSource::Staged { staged, .. } => staged.ordinal(),
            SealedSource::Landed(landed) => landed.ordinal(),
        };
        let utility = self
            .device(ordinal)
            .ok()
            .and_then(|device| device.utility.lock().ok()?.clone());
        if let Some(utility) = utility {
            fenced &= utility.synchronize().is_ok();
        }
        if fenced {
            return;
        }
        eprintln!("racer: stage=gpu ordinal={ordinal} state=ciphertext-quarantined");
        match source {
            SealedSource::Host(_) => {}
            SealedSource::Staged { staged, .. } => std::mem::forget(staged.clone()),
            SealedSource::Landed(landed) => std::mem::forget(landed.clone()),
        }
    }

    fn note_leaks(&self) {
        let leaked = self.gpu.leaked();
        if self.state.leaked.swap(leaked, Ordering::Relaxed) != leaked {
            eprintln!("racer: stage=gpu state=leaked total={leaked}");
        }
    }
}

/// State owned by the coordinator thread: discovery, imports, cuFile
/// registrations and reads, and landing buffers. Page work runs on the
/// executors.
struct Coordinator {
    engine: Arc<Engine>,
    /// Each device's GPU UUID, in the same order as `engine.devices`.
    uuids: Vec<[u8; 16]>,
    /// Registered files by (device, inode). The slab descriptors are few and
    /// long lived, so one registration serves many reads.
    files: HashMap<(u64, u64), u64>,
    /// Registered files by id, including retired ones that reads still use.
    open: HashMap<u64, OpenFile>,
    next_file: u64,
    /// Devices (`st_dev`) whose files cuFile rejected as unsupported. Reads
    /// of files on them use the host path without asking cuFile again.
    unsupported: std::collections::HashSet<u64>,
    /// Reads for the reader threads. `None` once they are told to stop.
    reads: Option<mpsc::Sender<FileJob>>,
    /// Reads the reader threads finished, so their files can be released.
    done: mpsc::Receiver<FileDone>,
    readers: Vec<std::thread::JoinHandle<()>>,
    /// Opens the cuFile driver ahead of the first sealed page.
    opener: Option<std::thread::JoinHandle<()>>,
    executors: Vec<std::thread::JoinHandle<()>>,
}

type Started = racer_gpu::Result<(Vec<DeviceInfo>, Vec<bool>, Vec<Arc<Queue>>)>;

fn coordinator_main(
    loader: Loader,
    jobs: mpsc::Receiver<Job>,
    ready: mpsc::SyncSender<Started>,
    state: HelperState,
) {
    let mut coordinator = match discover(loader) {
        Ok((gpu, contexts, usable)) => {
            // Before any reader or open: the flag applies only to the open
            // that loads cuFile.
            gpu.set_pci_p2pdma(state.pci_p2pdma);
            let decrypt = contexts.iter().map(probe_decrypt).collect();
            let _ = state.peers.set(enable_peers(&contexts, state.peer));
            let (reads, queue) = mpsc::channel();
            let queue = Arc::new(Mutex::new(queue));
            let (finished, done) = mpsc::channel();
            let mut readers = Vec::new();
            for _ in 0..FILE_READERS {
                let (reader, queue, finished, counters) = (
                    gpu.file_reader(),
                    queue.clone(),
                    finished.clone(),
                    state.counters.clone(),
                );
                // Without readers, stages fail and pages go through the host.
                if let Ok(thread) = std::thread::Builder::new()
                    .name("racer-gpu-read".into())
                    .spawn(move || reader_main(reader, queue, finished, counters))
                {
                    readers.push(thread);
                }
            }
            let opener = open_cufile(&gpu, &state.cufile, &state.counters);
            let uuids = usable
                .iter()
                .map(|device: &DeviceInfo| device.uuid)
                .collect();
            let devices: Vec<Arc<Device>> = contexts
                .into_iter()
                .map(|context| {
                    Arc::new(Device {
                        ordinal: context.ordinal(),
                        events: context.event_pool(IDLE_EVENTS),
                        context,
                        staging: Mutex::new(None),
                        utility: Mutex::new(None),
                        queue: Arc::new(Queue::new(state.queue_depth)),
                    })
                })
                .collect();
            let queues = devices.iter().map(|device| device.queue.clone()).collect();
            let engine = Arc::new(Engine {
                gpu,
                devices,
                landing: Mutex::new(HashMap::new()),
                state,
            });
            let executors = engine
                .devices
                .iter()
                .filter_map(|device| spawn_executor(&engine, device))
                .collect();
            let _ = ready.send(Ok((usable, decrypt, queues)));
            Coordinator {
                engine,
                uuids,
                files: HashMap::new(),
                open: HashMap::new(),
                next_file: 0,
                unsupported: std::collections::HashSet::new(),
                reads: (!readers.is_empty()).then_some(reads),
                done,
                readers,
                opener,
                executors,
            }
        }
        Err(error) => {
            let _ = ready.send(Err(error));
            return;
        }
    };
    drop(ready);
    while let Ok(job) = jobs.recv() {
        coordinator.finish_reads();
        coordinator.run(job);
    }
    // Every sender is gone: drain the executors, then let queued reads
    // finish and release their files.
    for device in &coordinator.engine.devices {
        device.queue.close();
    }
    for executor in std::mem::take(&mut coordinator.executors) {
        let _ = executor.join();
    }
    coordinator.reads = None;
    for reader in std::mem::take(&mut coordinator.readers) {
        let _ = reader.join();
    }
    if let Some(opener) = coordinator.opener.take() {
        let _ = opener.join();
    }
    coordinator.finish_reads();
    // A NIC registration may still cover landing memory: never free it.
    if let Ok(mut landing) = coordinator.engine.landing.lock() {
        for (_, buffer) in landing.drain() {
            std::mem::forget(buffer);
        }
    }
    coordinator.engine.note_leaks();
}

/// Start GPU `device`'s executor thread. If the thread cannot start, the
/// queue closes and pages for that GPU fail as unavailable.
fn spawn_executor(
    engine: &Arc<Engine>,
    device: &Arc<Device>,
) -> Option<std::thread::JoinHandle<()>> {
    let executor = Executor {
        engine: engine.clone(),
        device: device.clone(),
        lanes: (0..STREAMS).map(|_| Lane::default()).collect(),
        free_pairs: (0..scratch_pairs(engine.state.slots)).rev().collect(),
        waiting: std::collections::VecDeque::new(),
        requests: HashMap::new(),
        tainted: Vec::new(),
    };
    let spawned = std::thread::Builder::new()
        .name(format!("racer-gpu-{}", device.ordinal))
        .spawn(move || executor.run());
    if spawned.is_err() {
        eprintln!(
            "racer: stage=gpu ordinal={} state=executor-failed",
            device.ordinal
        );
        device.queue.close();
    }
    spawned.ok()
}

/// Open the cuFile driver on its own thread. Opening probes every GPU and can
/// take most of a second, so doing it at startup keeps that time out of the
/// first sealed page. A failed open turns cuFile off, as a failed stage does.
fn open_cufile(
    gpu: &racer_gpu::Gpu,
    cufile: &Arc<AtomicBool>,
    counters: &Arc<GpuCounters>,
) -> Option<std::thread::JoinHandle<()>> {
    if !cufile.load(Ordering::Relaxed) {
        return None;
    }
    let (reader, cufile) = (gpu.file_reader(), cufile.clone());
    let counters = counters.clone();
    std::thread::Builder::new()
        .name("racer-gpu-cufile".into())
        .spawn(move || {
            if let Err(error) = reader.open_driver()
                && cufile.swap(false, Ordering::Relaxed)
            {
                counters.cufile_disabled(CufileDisable::DriverOpen);
                eprintln!("racer: stage=gpu state=cufile-disabled reason={error}");
            }
        })
        .ok()
}

/// Enable peer access for every ordered pair of usable GPUs, unless `enabled`
/// is false, and log each pair once.
fn enable_peers(contexts: &[racer_gpu::Context], enabled: bool) -> PeerMatrix {
    let ordinals: Vec<i32> = contexts.iter().map(racer_gpu::Context::ordinal).collect();
    let mut allowed = Vec::with_capacity(contexts.len() * contexts.len());
    for dst in contexts {
        for src in contexts {
            let same = dst.ordinal() == src.ordinal();
            let state = if same {
                Ok(true)
            } else if enabled {
                dst.enable_peer(src)
            } else {
                Ok(false)
            };
            if !same {
                let status = match (&state, enabled) {
                    (_, false) => "off".to_string(),
                    (Ok(true), _) => "enabled".to_string(),
                    (Ok(false), _) => "unavailable".to_string(),
                    (Err(error), _) => format!("failed reason={error}"),
                };
                eprintln!(
                    "racer: stage=gpu ordinal={} peer={} state={status}",
                    dst.ordinal(),
                    src.ordinal()
                );
            }
            allowed.push(AtomicBool::new(matches!(state, Ok(true))));
        }
    }
    PeerMatrix { ordinals, allowed }
}

/// Queue a zero-length decrypt to learn whether the shim has working decrypt
/// kernels for this GPU. It writes nothing.
fn probe_decrypt(context: &racer_gpu::Context) -> bool {
    let (Ok(staging), Ok(stream)) = (context.staging(4096), context.stream()) else {
        return false;
    };
    let request = racer_gpu::DecryptRequest {
        key: &[0; 32],
        nonce: &[0; 12],
        aad: &[],
        length: 0,
    };
    let queued = stream.decrypt(
        &request,
        &staging,
        0,
        racer_gpu::Destination::Staging(&staging),
        64,
    );
    let synchronized = stream.synchronize();
    let verdict = synchronized.and_then(|()| stream.verdict());
    let usable = queued.is_ok() && verdict.is_ok_and(|v| v != racer_gpu::Verdict::NotRun);
    if !usable {
        eprintln!(
            "racer: stage=gpu ordinal={} decrypt=unavailable reason={:?}",
            context.ordinal(),
            queued.err()
        );
    }
    usable
}

type Discovered = (racer_gpu::Gpu, Vec<racer_gpu::Context>, Vec<DeviceInfo>);

/// Load the backend, log every GPU, and retain a context on each usable one.
fn discover(loader: Loader) -> racer_gpu::Result<Discovered> {
    let gpu = loader()?;
    let mut contexts = Vec::new();
    let mut usable = Vec::new();
    for device in gpu.devices()? {
        eprintln!(
            "racer: stage=gpu ordinal={} uuid={} pci_bus_id={} numa_node={} compute_mode={:?} usable={}",
            device.ordinal,
            device.uuid_string(),
            device.pci_bus_id,
            device.numa_node.map_or("unknown".into(), |n| n.to_string()),
            device.compute_mode,
            device.usable()
        );
        if !device.usable() {
            continue;
        }
        match gpu.context(device.ordinal) {
            Ok(context) => {
                contexts.push(context);
                usable.push(device);
            }
            Err(error) => eprintln!(
                "racer: stage=gpu ordinal={} state=skipped reason={error}",
                device.ordinal
            ),
        }
    }
    Ok((gpu, contexts, usable))
}

/// Map a GPU error on a write to a request error.
fn gpu_error(error: racer_gpu::Error) -> Error {
    match error {
        racer_gpu::Error::InvalidRange => Error::InvalidRange,
        _ => Error::Unavailable,
    }
}

/// The (device, inode) pair that names an open file.
fn file_identity(fd: &OwnedFd) -> Result<(u64, u64)> {
    use std::os::fd::AsRawFd;
    // SAFETY: fstat writes only into the zeroed stat buffer.
    let mut stat: libc::stat = unsafe { std::mem::zeroed() };
    if unsafe { libc::fstat(fd.as_raw_fd(), &mut stat) } != 0 {
        return Err(Error::Unavailable);
    }
    Ok((stat.st_dev, stat.st_ino))
}

/// Map a GPU error on import to an HTTP-visible error.
fn import_error(error: racer_gpu::Error) -> Error {
    match error {
        racer_gpu::Error::WrongDevice => Error::NotImplemented,
        racer_gpu::Error::InvalidRequest
        | racer_gpu::Error::InvalidRange
        | racer_gpu::Error::Driver(_) => Error::InvalidRequest,
        racer_gpu::Error::OutOfMemory | racer_gpu::Error::Busy => Error::Overloaded,
        _ => Error::Unavailable,
    }
}

impl Coordinator {
    fn run(&mut self, job: Job) {
        match job {
            Job::Import {
                id,
                fd,
                allocation_size,
                window,
                gpu_uuid,
                reply,
            } => {
                let result = self.import(id, fd, allocation_size, window, gpu_uuid);
                self.engine.note_leaks();
                let _ = reply.send(result);
            }
            Job::Stage {
                staged,
                fd,
                file_offset,
                length,
                reply,
            } => {
                let result = self.stage(staged, fd, file_offset, length, reply);
                self.engine.note_leaks();
                // A dropped receiver drops the slot, which frees it.
                if let Err((error, reply)) = result {
                    let _ = reply.send(Err(error));
                }
            }
            Job::Release { id, reply } => {
                let owner = self
                    .engine
                    .state
                    .owners
                    .lock()
                    .ok()
                    .and_then(|owners| owners.get(&id).copied());
                let forwarded = owner
                    .and_then(|ordinal| self.engine.device(ordinal).ok())
                    .map(|device| device.queue.clone());
                match forwarded {
                    Some(queue) => {
                        if let Err(error) = queue.control(Task::Release { id, reply }) {
                            eprintln!(
                                "racer: stage=gpu request={id} state=release-lost reason={error}"
                            );
                        }
                    }
                    // The import failed: there is nothing to release.
                    None => {
                        if let Some(reply) = reply {
                            let _ = reply.send(Ok(()));
                        }
                    }
                }
            }
            Job::Landing {
                id,
                ordinal,
                length,
                reply,
            } => {
                let result = self.landing(id, ordinal, length);
                let forget = |engine: &Engine| {
                    if let Ok(mut landing) = engine.landing.lock() {
                        landing.remove(&id);
                    }
                };
                if result.is_err() {
                    forget(&self.engine);
                }
                self.engine.note_leaks();
                // The caller stopped waiting, so no FreeLanding will come:
                // close the export and free the memory now.
                if let Err(Ok(fd)) = reply.send(result) {
                    drop(fd);
                    forget(&self.engine);
                }
            }
            Job::FreeLanding { id, fd } => {
                drop(fd);
                let removed = self
                    .engine
                    .landing
                    .lock()
                    .ok()
                    .and_then(|mut landing| landing.remove(&id));
                // Freed here unless a running page still holds it.
                if removed.is_some() {
                    drop(removed);
                    eprintln!("racer: stage=gpu-landing allocation={id} state=freed");
                }
                self.engine.note_leaks();
            }
            #[cfg(test)]
            Job::Run(f) => f(),
        }
    }

    /// Allocate landing buffer `id` on GPU `ordinal`, and export it.
    fn landing(&mut self, id: u64, ordinal: i32, length: u64) -> Result<OwnedFd> {
        let mut landing = self.engine.landing.lock().map_err(|_| Error::Internal)?;
        let buffer = match landing.entry(id) {
            std::collections::hash_map::Entry::Occupied(entry) => entry.into_mut(),
            std::collections::hash_map::Entry::Vacant(entry) => {
                let device = self
                    .engine
                    .device(ordinal)
                    .map_err(|_| Error::Unavailable)?;
                let buffer = device.context.staging(length).map_err(|error| {
                    eprintln!(
                        "racer: stage=gpu ordinal={ordinal} state=landing-failed reason={error}"
                    );
                    Error::Overloaded
                })?;
                entry.insert(Arc::new(buffer))
            }
        };
        if buffer.size() != length {
            return Err(Error::InvalidConfiguration);
        }
        buffer.dma_buf(0, length).map_err(|error| {
            eprintln!(
                "racer: stage=gpu ordinal={ordinal} state=landing-export-failed reason={error}"
            );
            Error::Unavailable
        })
    }

    /// Import the allocation on the GPU that owns it and hand it to that
    /// GPU's executor. With the client's GPU UUID, that GPU is tried first:
    /// device numbers differ between the client's container and the
    /// dataplane, but UUIDs do not. An unknown UUID is a GPU the dataplane
    /// cannot use, so the read falls back to the host (501). Without a UUID
    /// (older clients), each usable GPU is tried until one owns the
    /// allocation.
    fn import(
        &mut self,
        id: u64,
        fd: OwnedFd,
        size: u64,
        window: Range<u64>,
        gpu_uuid: Option<[u8; 16]>,
    ) -> Result<i32> {
        let devices = &self.engine.devices;
        let mut order: Vec<usize> = (0..devices.len()).collect();
        if let Some(uuid) = gpu_uuid {
            let Some(first) = self.uuids.iter().position(|known| *known == uuid) else {
                eprintln!(
                    "racer: stage=gpu state=import-skipped reason=unknown-gpu-uuid uuid={}",
                    racer_gpu::uuid_string(&uuid)
                );
                return Err(Error::NotImplemented);
            };
            order.retain(|&index| index != first);
            order.insert(0, first);
        }
        let mut last = racer_gpu::Error::WrongDevice;
        for (attempt, index) in order.into_iter().enumerate() {
            let device = &devices[index];
            let handle = fd.try_clone().map_err(|_| Error::Unavailable)?;
            match device.context.import(handle, size, window.clone()) {
                Ok(import) => {
                    if attempt > 0 && gpu_uuid.is_some() {
                        eprintln!(
                            "racer: stage=gpu state=uuid-mismatch ordinal={} uuid={}",
                            device.ordinal,
                            racer_gpu::uuid_string(&self.uuids[index])
                        );
                    }
                    // Registered before the executor can release it.
                    if let Ok(mut owners) = self.engine.state.owners.lock() {
                        owners.insert(id, device.ordinal);
                    }
                    let adopted = device.queue.control(Task::Adopt {
                        id,
                        import: Arc::new(import),
                    });
                    if let Err(error) = adopted {
                        if let Ok(mut owners) = self.engine.state.owners.lock() {
                            owners.remove(&id);
                        }
                        return Err(error);
                    }
                    return Ok(device.ordinal);
                }
                Err(racer_gpu::Error::WrongDevice) => continue,
                Err(error) => {
                    last = error;
                    break;
                }
            }
        }
        Err(import_error(last))
    }

    /// Queue a cuFile read of a file range into a staged slot. A reader
    /// thread answers `reply`; on an early error it comes back to the caller.
    fn stage(
        &mut self,
        staged: StagedCiphertext,
        fd: OwnedFd,
        file_offset: u64,
        length: u64,
        reply: oneshot::Sender<Result<StagedCiphertext>>,
    ) -> std::result::Result<(), (Error, oneshot::Sender<Result<StagedCiphertext>>)> {
        let Some(reads) = self.reads.clone() else {
            return Err((Error::Unavailable, reply));
        };
        let identity = match file_identity(&fd) {
            Ok(identity) => identity,
            Err(error) => return Err((error, reply)),
        };
        if self.unsupported.contains(&identity.0) {
            return Err((Error::Unavailable, reply));
        }
        let staging = match self.engine.staging(staged.ordinal) {
            Ok(staging) => staging,
            Err(error) => return Err((error, reply)),
        };
        if let Err(error) = staging.register_cufile() {
            self.engine
                .state
                .counters
                .file_failures
                .fetch_add(1, Ordering::Relaxed);
            self.disable_cufile(error, CufileDisable::StagingRegister);
            return Err((Error::Unavailable, reply));
        }
        let id = match self.files.get(&identity) {
            Some(id) => *id,
            None => {
                if self.files.len() >= MAX_FILES {
                    for id in std::mem::take(&mut self.files).into_values() {
                        self.retire(id);
                    }
                }
                match self.engine.gpu.open_file(fd) {
                    Ok(file) => {
                        let id = self.next_file;
                        self.next_file += 1;
                        self.open.insert(
                            id,
                            OpenFile {
                                file,
                                reads: 0,
                                retired: false,
                            },
                        );
                        self.files.insert(identity, id);
                        id
                    }
                    Err(error) => {
                        self.engine
                            .state
                            .counters
                            .file_failures
                            .fetch_add(1, Ordering::Relaxed);
                        if let racer_gpu::Error::Unsupported(_) = error {
                            self.unsupported_device(identity.0, error);
                            return Err((Error::Unavailable, reply));
                        }
                        self.disable_cufile(error, CufileDisable::FileOpen);
                        return Err((Error::Unavailable, reply));
                    }
                }
            }
        };
        let Some(open) = self.open.get_mut(&id) else {
            return Err((Error::Internal, reply));
        };
        // SAFETY: the file stays in `open` while `reads` is above zero, the
        // job holds the staging buffer, the backend lives until the readers
        // are joined, and the slot is this job's alone until the read
        // answers.
        let read = match unsafe {
            open.file
                .detach(&staging, staged.offset(), file_offset, length)
        } {
            Ok(read) => read,
            Err(error) => return Err((gpu_error(error), reply)),
        };
        open.reads += 1;
        let job = FileJob {
            read,
            file: id,
            staged,
            _staging: staging,
            reply,
        };
        if let Err(mpsc::SendError(job)) = reads.send(job) {
            open.reads -= 1;
            return Err((Error::Unavailable, job.reply));
        }
        Ok(())
    }

    /// Account for reads the reader threads finished. A failed handle may be
    /// stale, so it is registered again next time.
    fn finish_reads(&mut self) {
        while let Ok(FileDone { file, failed }) = self.done.try_recv() {
            if let Some(open) = self.open.get_mut(&file) {
                open.reads = open.reads.saturating_sub(1);
            }
            if failed {
                self.files.retain(|_, id| *id != file);
                self.retire(file);
            } else {
                self.release_file(file);
            }
        }
    }

    /// Stop handing out a file; free it once no read uses it.
    fn retire(&mut self, id: u64) {
        if let Some(open) = self.open.get_mut(&id) {
            open.retired = true;
        }
        self.release_file(id);
    }

    /// Free a retired file that no read uses.
    fn release_file(&mut self, id: u64) {
        if self
            .open
            .get(&id)
            .is_some_and(|open| open.retired && open.reads == 0)
        {
            self.open.remove(&id);
        }
    }

    /// Stop trying cuFile for files on `device` after cuFile rejected one of
    /// them. Other devices keep using cuFile.
    fn unsupported_device(&mut self, device: u64, error: racer_gpu::Error) {
        if self.unsupported.insert(device) {
            self.engine
                .state
                .counters
                .cufile_unsupported
                .fetch_add(1, Ordering::Relaxed);
            let (major, minor) = (libc::major(device), libc::minor(device));
            eprintln!(
                "racer: stage=gpu state=cufile-unsupported dev={major}:{minor} reason={error}"
            );
        }
    }

    /// Stop trying cuFile after a failure that is not about one read.
    fn disable_cufile(&self, error: racer_gpu::Error, reason: CufileDisable) {
        let state = &self.engine.state;
        if state.cufile.swap(false, Ordering::Relaxed) {
            state.counters.cufile_disabled(reason);
            eprintln!("racer: stage=gpu state=cufile-disabled reason={error}");
        }
    }
}

/// A request imported on this executor's GPU.
struct Owned {
    import: Arc<racer_gpu::ImportedAllocation>,
    /// A page of it is running on a stream.
    busy: bool,
    /// A release that waits for its pages: `Some(reply)` once asked.
    release: Option<Option<oneshot::Sender<Result<()>>>>,
}

/// One stream of an executor and the page running on it.
#[derive(Default)]
struct Lane {
    /// Made on first use; `None` again after it was set aside as tainted.
    stream: Option<racer_gpu::Stream>,
    /// This stream's page-locked host slot: `None` until first use, then
    /// `Some(None)` if allocation failed (copies read pageable memory).
    pinned: Option<Option<racer_gpu::PinnedBuffer>>,
    running: Option<Running>,
}

/// A page whose GPU work is queued on a lane.
struct Running {
    task: WriteTask,
    /// Released, in the fence, only once the work completes.
    import: Arc<racer_gpu::ImportedAllocation>,
    /// The scratch pair the page uses, if any.
    pair: Option<u64>,
    fence: racer_gpu::Fence,
    step: Step,
    /// Ciphertext came from another GPU: `(source GPU, bytes)` over the peer
    /// path, or bounced through host memory.
    peer: Option<(i32, u64)>,
    bounced: bool,
    /// The source GPU's fence for a bounce, kept until the page is done.
    _bounce: Option<racer_gpu::Fence>,
}

/// What the fence of a running page marks.
enum Step {
    /// A plaintext copy into the import.
    Copied,
    /// A decrypt; `whole` wrote the import, otherwise the plaintext scratch
    /// slot.
    Decrypted { whole: bool },
    /// The copy of a verified partial page out of scratch.
    Tail,
}

/// A stream set aside after a failed page could not be fenced, with the
/// scratch pair its queued work may still write.
struct Tainted {
    stream: racer_gpu::Stream,
    pair: Option<u64>,
}

/// Runs one GPU's page jobs: up to [`STREAMS`] at once, each on its own
/// stream, and completes them by polling fences.
struct Executor {
    engine: Arc<Engine>,
    device: Arc<Device>,
    lanes: Vec<Lane>,
    /// Scratch pairs not held by a running page or a tainted stream.
    free_pairs: Vec<u64>,
    /// Page jobs not started yet, in arrival order.
    waiting: std::collections::VecDeque<WriteTask>,
    requests: HashMap<u64, Owned>,
    tainted: Vec<Tainted>,
}

/// Offsets of scratch pair `pair`: ciphertext, then partial plaintext.
fn scratch(pair: u64) -> (u64, u64) {
    (2 * pair * SLOT_BYTES, (2 * pair + 1) * SLOT_BYTES)
}

impl Executor {
    fn run(mut self) {
        let _thread = self.engine.gpu.enter_thread();
        let mut nap = POLL_MIN;
        loop {
            let running = self.lanes.iter().any(|lane| lane.running.is_some());
            // Idle: sleep until a message. Otherwise nap briefly between
            // fence polls; a message still wakes the executor at once.
            let wait = (running || !self.waiting.is_empty()).then_some(nap);
            let (tasks, closed) = self.device.queue.take(wait);
            let arrived = !tasks.is_empty();
            for task in tasks {
                self.accept(task);
            }
            let completed = self.poll();
            self.dispatch();
            let running = self.lanes.iter().any(|lane| lane.running.is_some());
            if closed && !running && self.waiting.is_empty() {
                break;
            }
            nap = if arrived || completed {
                POLL_MIN
            } else {
                (nap * 2).min(POLL_MAX)
            };
        }
        self.shutdown();
    }

    fn accept(&mut self, task: Task) {
        match task {
            Task::Adopt { id, import } => {
                self.requests.insert(
                    id,
                    Owned {
                        import,
                        busy: false,
                        release: None,
                    },
                );
            }
            Task::Write(task) => self.waiting.push_back(task),
            Task::Release { id, reply } => {
                let pending = self.waiting.iter().any(|task| task.id == id);
                match self.requests.get_mut(&id) {
                    Some(owned) if owned.busy || pending => owned.release = Some(reply),
                    Some(_) => self.release(id, reply),
                    None => {
                        if let Some(reply) = reply {
                            let _ = reply.send(Ok(()));
                        }
                    }
                }
            }
        }
    }

    /// Drop request `id`'s import and answer `reply`.
    fn release(&mut self, id: u64, reply: Option<oneshot::Sender<Result<()>>>) {
        if let Ok(mut owners) = self.engine.state.owners.lock() {
            owners.remove(&id);
        }
        let before = self.engine.gpu.leaked();
        drop(self.requests.remove(&id));
        self.engine.note_leaks();
        let result = if self.engine.gpu.leaked() == before {
            Ok(())
        } else {
            Err(Error::Unavailable)
        };
        if let Some(reply) = reply {
            let _ = reply.send(result);
        }
    }

    /// Advance every running page whose fence completed. Returns whether
    /// any did.
    fn poll(&mut self) -> bool {
        let mut completed = false;
        for index in 0..self.lanes.len() {
            let Some(running) = self.lanes[index].running.as_ref() else {
                continue;
            };
            match running.fence.poll() {
                Ok(false) => {}
                Ok(true) => {
                    completed = true;
                    self.advance(index);
                }
                Err(error) => {
                    completed = true;
                    let running = self.lanes[index].running.take().expect("running");
                    eprintln!(
                        "racer: stage=gpu ordinal={} state=page-failed reason={error}",
                        self.device.ordinal
                    );
                    if let Some((src, _)) = running.peer {
                        self.peer_failed(src, &error);
                    }
                    self.fail(index, running.task, running.pair, Error::Unavailable);
                }
            }
        }
        completed
    }

    /// The work behind lane `index`'s fence completed: answer the page, or
    /// queue its next step.
    fn advance(&mut self, index: usize) {
        let mut running = self.lanes[index].running.take().expect("running");
        let counters = self.engine.state.counters.clone();
        match running.step {
            Step::Copied => {
                counters.plaintext.fetch_add(1, Ordering::Relaxed);
                self.finish(index, running, Ok(()));
            }
            Step::Decrypted { whole } => {
                if let Some((_, bytes)) = running.peer {
                    counters.peer_copies.fetch_add(1, Ordering::Relaxed);
                    counters.peer_copy_bytes.fetch_add(bytes, Ordering::Relaxed);
                }
                if running.bounced {
                    counters.peer_bounces.fetch_add(1, Ordering::Relaxed);
                }
                running.peer = None;
                running.bounced = false;
                let stream = self.lanes[index].stream.as_ref().expect("lane stream");
                let verdict = stream.verdict();
                match verdict {
                    Ok(racer_gpu::Verdict::Passed) if whole => self.decrypted(index, running),
                    Ok(racer_gpu::Verdict::Passed) => {
                        // Only now, after the verdict, may scratch plaintext
                        // reach the import.
                        match self.queue_tail(index, &running) {
                            Ok(fence) => {
                                running.fence = fence;
                                running.step = Step::Tail;
                                self.lanes[index].running = Some(running);
                            }
                            Err(error) => self.fail(index, running.task, running.pair, error),
                        }
                    }
                    Ok(racer_gpu::Verdict::TagMismatch) => {
                        counters.rejected.fetch_add(1, Ordering::Relaxed);
                        self.finish(index, running, Err(Error::CorruptRecord));
                    }
                    Ok(racer_gpu::Verdict::NotRun) => {
                        self.finish(index, running, Err(Error::Unavailable))
                    }
                    Err(error) => {
                        self.fail(index, running.task, running.pair, gpu_error(error));
                    }
                }
            }
            Step::Tail => self.decrypted(index, running),
        }
    }

    /// A sealed page was verified and written.
    fn decrypted(&mut self, index: usize, running: Running) {
        let counters = &self.engine.state.counters;
        counters.decrypted.fetch_add(1, Ordering::Relaxed);
        if let Work::Sealed(sealed) = &running.task.work
            && matches!(sealed.page.source, SealedSource::Landed(_))
        {
            counters.landed.fetch_add(1, Ordering::Relaxed);
        }
        self.finish(index, running, Ok(()));
    }

    /// Queue the copy of a verified partial page from scratch into the
    /// import, and fence it.
    fn queue_tail(&mut self, index: usize, running: &Running) -> Result<racer_gpu::Fence> {
        let Work::Sealed(sealed) = &running.task.work else {
            return Err(Error::Internal);
        };
        let (_, plaintext) = scratch(running.pair.ok_or(Error::Internal)?);
        let staging = self.engine.staging(self.device.ordinal)?;
        let import = &running.import;
        let stream = self.lanes[index].stream.as_ref().ok_or(Error::Internal)?;
        stream
            .copy_from_staging(
                racer_gpu::Destination::Import(import),
                running.task.offset,
                &staging,
                plaintext + sealed.range.start,
                sealed.range.end - sealed.range.start,
            )
            .map_err(gpu_error)?;
        stream.fence_in(&self.device.events).map_err(gpu_error)
    }

    /// Answer a page whose work completed, and free its lane and scratch.
    fn finish(&mut self, index: usize, running: Running, result: Result<()>) {
        let Running { task, pair, .. } = running;
        if let Some(pair) = pair {
            self.free_pairs.push(pair);
        }
        self.answer(task, result);
        debug_assert!(self.lanes[index].running.is_none());
    }

    /// Answer `task`, then run a release that waited for it.
    fn answer(&mut self, task: WriteTask, result: Result<()>) {
        let WriteTask {
            id, work, reply, ..
        } = task;
        // If the I/O side stopped waiting, the page drops here. Its storage
        // is shared and thread safe, so that is fine.
        // Count the page done before answering, so a caller that saw the
        // answer never sees it still queued.
        self.device.queue.finished();
        let _ = reply.send((work, result));
        let pending = self.waiting.iter().any(|task| task.id == id);
        if let Some(owned) = self.requests.get_mut(&id) {
            owned.busy = false;
            if !pending && let Some(reply) = owned.release.take() {
                self.release(id, reply);
            }
        }
    }

    /// Fail a page after some of its work may have been queued on lane
    /// `index`. The lane's stream is synchronized first, so nothing queued
    /// still reads the source or writes scratch. If that fails, the stream
    /// and its scratch pair are set aside (tainted) until a later
    /// synchronize succeeds, and the page's staged or landed source is
    /// quarantined unless another fence covers it.
    fn fail(&mut self, index: usize, task: WriteTask, pair: Option<u64>, error: Error) {
        let lane = &mut self.lanes[index];
        let fenced = lane
            .stream
            .as_ref()
            .is_none_or(|stream| stream.synchronize().is_ok());
        if fenced {
            if let Some(pair) = pair {
                self.free_pairs.push(pair);
            }
        } else if let Some(stream) = lane.stream.take() {
            if self.tainted.is_empty() {
                eprintln!(
                    "racer: stage=gpu ordinal={} state=scratch-tainted",
                    self.device.ordinal
                );
            }
            self.tainted.push(Tainted { stream, pair });
        }
        if let Work::Sealed(sealed) = &task.work {
            self.engine.fence_source(fenced, &sealed.page.source);
        }
        self.engine.note_leaks();
        self.answer(task, Err(error));
    }

    /// Turn the peer path from `src` off after a failed copy.
    fn peer_failed(&self, src: i32, error: &racer_gpu::Error) {
        let dst = self.device.ordinal;
        let state = &self.engine.state;
        state.counters.peer_failures.fetch_add(1, Ordering::Relaxed);
        let entry = state.peers.get().and_then(|matrix| matrix.entry(dst, src));
        if entry.is_some_and(|allowed| allowed.swap(false, Ordering::Relaxed)) {
            eprintln!(
                "racer: stage=gpu ordinal={dst} peer={src} state=peer-disabled reason={error}"
            );
        }
    }

    /// Refuse sealed pages while a tainted stream may still write scratch.
    /// Each tainted stream is synchronized again first; those that succeed
    /// give their scratch pair back and are freed.
    fn check_scratch(&mut self) -> Result<()> {
        if self.tainted.is_empty() {
            return Ok(());
        }
        let mut kept = Vec::new();
        for tainted in std::mem::take(&mut self.tainted) {
            if tainted.stream.synchronize().is_ok() {
                if let Some(pair) = tainted.pair {
                    self.free_pairs.push(pair);
                }
                drop(tainted.stream);
                self.engine.state.streams.fetch_sub(1, Ordering::Relaxed);
            } else {
                kept.push(tainted);
            }
        }
        self.tainted = kept;
        if !self.tainted.is_empty() {
            return Err(Error::Unavailable);
        }
        eprintln!(
            "racer: stage=gpu ordinal={} state=scratch-fenced",
            self.device.ordinal
        );
        self.engine.note_leaks();
        Ok(())
    }

    /// Start waiting pages on idle lanes, in arrival order. A page waits
    /// while its request has another page running, or while it needs a
    /// scratch pair and none is free; later pages may pass it.
    fn dispatch(&mut self) {
        let mut position = 0;
        while position < self.waiting.len() {
            let Some(index) = self.lanes.iter().position(|lane| lane.running.is_none()) else {
                return;
            };
            let task = &self.waiting[position];
            if self.requests.get(&task.id).is_some_and(|owned| owned.busy) {
                position += 1;
                continue;
            }
            let sealed = matches!(task.work, Work::Sealed(_));
            let needs = needs_scratch(task, self.device.ordinal);
            if sealed && let Err(error) = self.check_scratch() {
                let task = self.waiting.remove(position).expect("present");
                self.answer(task, Err(error));
                continue;
            }
            let pair = if needs {
                match self.free_pairs.pop() {
                    Some(pair) => Some(pair),
                    None => {
                        position += 1;
                        continue;
                    }
                }
            } else {
                None
            };
            let task = self.waiting.remove(position).expect("present");
            self.begin(index, task, pair);
        }
    }

    /// Queue `task`'s first step on lane `index`.
    fn begin(&mut self, index: usize, task: WriteTask, pair: Option<u64>) {
        let Some(owned) = self.requests.get_mut(&task.id) else {
            if let Some(pair) = pair {
                self.free_pairs.push(pair);
            }
            self.answer(task, Err(Error::Internal));
            return;
        };
        owned.busy = true;
        let import = owned.import.clone();
        if self.lanes[index].stream.is_none() {
            match self.device.context.stream() {
                Ok(stream) => {
                    self.engine.state.streams.fetch_add(1, Ordering::Relaxed);
                    self.lanes[index].stream = Some(stream);
                }
                Err(error) => {
                    if let Some(pair) = pair {
                        self.free_pairs.push(pair);
                    }
                    self.answer(task, Err(gpu_error(error)));
                    return;
                }
            }
        }
        let queued = match &task.work {
            Work::Plaintext(plaintext) => self.queue_plaintext(index, &task, plaintext, &import),
            Work::Sealed(sealed) => self.queue_sealed(index, &task, sealed, &import, pair),
        };
        match queued {
            Ok(queued) => {
                self.lanes[index].running = Some(Running {
                    task,
                    import,
                    pair,
                    fence: queued.fence,
                    step: queued.step,
                    peer: queued.peer,
                    bounced: queued.bounced,
                    _bounce: queued.bounce,
                });
            }
            Err(error) => self.fail(index, task, pair, error),
        }
    }

    /// Lane `index`'s pinned slot holding a copy of `bytes`, or `None` to
    /// copy from pageable memory instead.
    fn pinned_for(&mut self, index: usize, bytes: &[u8]) -> Option<racer_gpu::PinnedBuffer> {
        let pinned = self.pinned_slot(index)?;
        pinned.write(0, bytes).ok()?;
        Some(pinned)
    }

    /// Lane `index`'s pinned slot, allocated on first use.
    fn pinned_slot(&mut self, index: usize) -> Option<racer_gpu::PinnedBuffer> {
        let lane = &mut self.lanes[index];
        if lane.pinned.is_none() {
            let ordinal = self.device.ordinal;
            let slot = self.device.context.pinned(SLOT_BYTES).map_err(|error| {
                eprintln!("racer: stage=gpu ordinal={ordinal} state=pinned-failed reason={error}");
            });
            lane.pinned = Some(slot.ok());
        }
        lane.pinned.clone().flatten()
    }

    fn queue_plaintext(
        &mut self,
        index: usize,
        task: &WriteTask,
        plaintext: &Plaintext,
        import: &racer_gpu::ImportedAllocation,
    ) -> Result<Queued> {
        let pinned = self.pinned_for(index, plaintext.bytes());
        let stream = self.lanes[index].stream.as_ref().ok_or(Error::Internal)?;
        let destination = racer_gpu::Destination::Import(import);
        let length = plaintext.bytes().len() as u64;
        match &pinned {
            Some(pinned) => stream.copy_from_pinned(destination, task.offset, pinned, 0, length),
            None => stream.copy_from_host(destination, task.offset, plaintext.clone()),
        }
        .map_err(gpu_error)?;
        Ok(Queued {
            fence: stream.fence_in(&self.device.events).map_err(gpu_error)?,
            step: Step::Copied,
            peer: None,
            bounced: false,
            bounce: None,
        })
    }

    /// Queue the copies a sealed page needs, then its decrypt, and fence
    /// them. Nothing waits on the host except an RDMA flush and the
    /// pageable bounce fallback.
    fn queue_sealed(
        &mut self,
        index: usize,
        task: &WriteTask,
        sealed: &Sealed,
        import: &racer_gpu::ImportedAllocation,
        pair: Option<u64>,
    ) -> Result<Queued> {
        let ordinal = self.device.ordinal;
        let staging = self.engine.staging(ordinal)?;
        let page = &sealed.page;
        let length = u64::from(page.plaintext_length());
        if sealed.range.start >= sealed.range.end || sealed.range.end > length {
            return Err(Error::InvalidRange);
        }
        if let SealedSource::Landed(landed) = &page.source {
            if landed.len() != length + 16 {
                return Err(Error::Internal);
            }
            // The NIC wrote these bytes; the decrypt reads them twice
            // (GHASH, then CTR), so they must be visible and final first.
            self.engine
                .flush_landed(landed.ordinal(), landed.ordinal() != ordinal)?;
        }
        let remote = match &page.source {
            SealedSource::Staged { staged, .. } if staged.ordinal() != ordinal => Some(Remote {
                ordinal: staged.ordinal(),
                landed: None,
                offset: staged.offset(),
            }),
            SealedSource::Landed(landed) if landed.ordinal() != ordinal => Some(Remote {
                ordinal: landed.ordinal(),
                landed: Some(landed.allocation),
                offset: landed.offset(),
            }),
            _ => None,
        };
        let whole = sealed.range.start == 0 && sealed.range.end == length;
        let (ciphertext, plaintext) = match pair {
            Some(pair) => scratch(pair),
            None if remote.is_none() && whole && !matches!(page.source, SealedSource::Host(_)) => {
                (0, 0)
            }
            None => return Err(Error::Internal),
        };
        let mut peer = None;
        let mut bounced = false;
        let mut bounce = None;
        if let Some(remote) = &remote {
            let buffer = self.engine.source_buffer(remote)?;
            let src = remote.ordinal;
            let allowed = self
                .engine
                .state
                .peers
                .get()
                .and_then(|matrix| matrix.entry(ordinal, src))
                .is_some_and(|allowed| allowed.load(Ordering::Relaxed));
            let mut copied = false;
            if allowed {
                let stream = self.lanes[index].stream.as_ref().ok_or(Error::Internal)?;
                match stream.copy_from_peer(
                    racer_gpu::Destination::Staging(&staging),
                    ciphertext,
                    &buffer,
                    remote.offset,
                    length + 16,
                ) {
                    Ok(()) => {
                        peer = Some((src, length + 16));
                        copied = true;
                    }
                    // Part of it may have been queued; the bounce below
                    // is queued after it on the same stream, so it wins.
                    Err(error) => self.peer_failed(src, &error),
                }
            }
            if !copied {
                bounce = self.bounce(index, remote, &buffer, &staging, ciphertext, length + 16)?;
                bounced = true;
            }
        }
        let pinned = match &page.source {
            SealedSource::Host(copy) if remote.is_none() => {
                self.pinned_for(index, copy.copy.ciphertext.bytes())
            }
            _ => None,
        };
        let stream = self.lanes[index].stream.as_ref().ok_or(Error::Internal)?;
        let landing;
        let (source_buffer, source) = match &page.source {
            _ if remote.is_some() => (&staging, ciphertext),
            SealedSource::Host(copy) => {
                let bytes = copy.copy.ciphertext.bytes();
                if bytes.len() as u64 != length + 16 {
                    return Err(Error::CorruptRecord);
                }
                let destination = racer_gpu::Destination::Staging(&staging);
                match &pinned {
                    Some(pinned) => {
                        stream.copy_from_pinned(destination, ciphertext, pinned, 0, length + 16)
                    }
                    None => stream.copy_from_host(
                        destination,
                        ciphertext,
                        HostCiphertext(copy.copy.ciphertext.clone()),
                    ),
                }
                .map_err(gpu_error)?;
                (&staging, ciphertext)
            }
            SealedSource::Staged { staged, .. } => (&staging, staged.offset()),
            SealedSource::Landed(landed) => {
                landing = self
                    .engine
                    .landing
                    .lock()
                    .map_err(|_| Error::Internal)?
                    .get(&landed.allocation)
                    .cloned()
                    .ok_or(Error::Internal)?;
                (&landing, landed.offset())
            }
        };
        let request = racer_gpu::DecryptRequest {
            key: page.key.key(),
            nonce: page.key.nonce(),
            aad: &page.aad,
            length,
        };
        // A whole page decrypts straight into the import. Part of a page
        // decrypts into scratch first, because the tag covers the whole page
        // and nothing outside the window may be written.
        let (destination, at) = if whole {
            (racer_gpu::Destination::Import(import), task.offset)
        } else {
            (racer_gpu::Destination::Staging(&staging), plaintext)
        };
        let decrypted = stream.decrypt(&request, source_buffer, source, destination, at);
        if let Err(racer_gpu::Error::KernelUnavailable) = decrypted {
            if let Ok(mut off) = self.engine.state.kernels_off.lock() {
                off.insert(ordinal);
            }
            return Err(Error::Unavailable);
        }
        decrypted.map_err(gpu_error)?;
        Ok(Queued {
            fence: stream.fence_in(&self.device.events).map_err(gpu_error)?,
            step: Step::Decrypted { whole },
            peer,
            bounced,
            bounce,
        })
    }

    /// Move ciphertext from another GPU into this GPU's scratch through
    /// host memory. With a free pinned slot this waits on nothing: the
    /// source GPU's utility stream copies into the slot and the lane's
    /// stream waits on that fence before copying it in. The returned fence
    /// holds the slot until the page is done. Without one, the source is
    /// read back synchronously (pageable fallback).
    fn bounce(
        &mut self,
        index: usize,
        remote: &Remote,
        buffer: &racer_gpu::StagingBuffer,
        staging: &racer_gpu::StagingBuffer,
        ciphertext: u64,
        length: u64,
    ) -> Result<Option<racer_gpu::Fence>> {
        let utility = self.engine.utility(remote.ordinal)?;
        let events = self.engine.device(remote.ordinal)?.events.clone();
        let pinned = self.pinned_slot(index).filter(|pinned| !pinned.in_flight());
        let stream = self.lanes[index].stream.as_ref().ok_or(Error::Internal)?;
        let destination = racer_gpu::Destination::Staging(staging);
        if let Some(pinned) = pinned {
            let fence = utility
                .copy_to_pinned(buffer, remote.offset, &pinned, 0, length)
                .and_then(|()| utility.fence_in(&events))
                .map_err(gpu_error)?;
            stream
                .wait_for(&fence)
                .and_then(|()| stream.copy_from_pinned(destination, ciphertext, &pinned, 0, length))
                .map_err(gpu_error)?;
            return Ok(Some(fence));
        }
        let mut bytes = vec![0; usize::try_from(length).map_err(|_| Error::Internal)?];
        utility
            .read_staging(buffer, remote.offset, &mut bytes)
            .map_err(gpu_error)?;
        stream
            .copy_from_host(destination, ciphertext, bytes)
            .map_err(gpu_error)?;
        Ok(None)
    }

    /// Release everything once the queue closed and every page answered.
    fn shutdown(&mut self) {
        let ids: Vec<u64> = self.requests.keys().copied().collect();
        for id in ids {
            self.release(id, None);
        }
        let streams = &self.engine.state.streams;
        for lane in self.lanes.drain(..) {
            if lane.stream.is_some() {
                streams.fetch_sub(1, Ordering::Relaxed);
            }
        }
        for tainted in self.tainted.drain(..) {
            drop(tainted.stream);
            streams.fetch_sub(1, Ordering::Relaxed);
        }
        self.engine.note_leaks();
    }
}

/// The first step of a page, queued.
struct Queued {
    fence: racer_gpu::Fence,
    step: Step,
    peer: Option<(i32, u64)>,
    bounced: bool,
    bounce: Option<racer_gpu::Fence>,
}

/// Whether `task` on GPU `ordinal` needs a scratch pair: host ciphertext
/// and ciphertext from another GPU are copied into scratch first, and part
/// of a page is decrypted into scratch before the copy out.
fn needs_scratch(task: &WriteTask, ordinal: i32) -> bool {
    let Work::Sealed(sealed) = &task.work else {
        return false;
    };
    let length = u64::from(sealed.page.plaintext_length());
    let whole = sealed.range.start == 0 && sealed.range.end == length;
    let local = match &sealed.page.source {
        SealedSource::Host(_) => false,
        SealedSource::Staged { staged, .. } => staged.ordinal() == ordinal,
        SealedSource::Landed(landed) => landed.ordinal() == ordinal,
    };
    !(whole && local)
}

#[cfg(test)]
pub(crate) mod testing {
    //! A GPU helper on simulated GPUs, for tests outside the client socket.
    use super::*;
    use std::cell::RefCell;

    thread_local! {
        /// The simulation, on the GPU coordinator thread only.
        static SIM: RefCell<Option<(racer_gpu::simulation::Simulation, racer_gpu::simulation::Environment)>> =
            const { RefCell::new(None) };
    }

    /// The coordinator thread's simulation. Call through [`GpuHelper::run`].
    pub(crate) fn simulation() -> racer_gpu::simulation::Simulation {
        SIM.with(|sim| sim.borrow().as_ref().unwrap().0.clone())
    }

    /// Live simulated staging and landing buffers. Run on the coordinator thread.
    pub(crate) fn live_staging() -> usize {
        simulation().live().staging
    }

    /// A test scope with a few seconds on the real clock.
    pub(crate) fn scope() -> RequestScope {
        RequestScope::host(
            crate::model::RequestId([1; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(5),
        )
        .unwrap()
    }

    /// GPU `ordinal`'s landing buffer of `length` bytes, blocking.
    pub(crate) fn landing(
        helper: &GpuHelper,
        ordinal: i32,
        length: u64,
    ) -> Result<Arc<GpuLanding>> {
        futures::executor::block_on(helper.landing(ordinal, length, &scope()))
    }

    /// Write `bytes` at `at` into a landing buffer through its dma-buf, as
    /// a NIC would.
    pub(crate) fn nic_write(landing: &GpuLanding, at: u64, bytes: &[u8]) {
        use std::os::unix::fs::FileExt;
        let fd = landing.allocation.fd.as_ref().unwrap();
        let file = std::fs::File::from(fd.try_clone().unwrap());
        file.write_all_at(bytes, landing.offset + at).unwrap();
    }

    /// Import a fresh allocation on GPU `ordinal` as request `id`.
    pub(crate) fn import(helper: &GpuHelper, id: u64, ordinal: i32) -> OwnedFd {
        let size = racer_gpu::simulation::GRANULARITY;
        let fd = helper.run(move || simulation().allocate(ordinal, size));
        let (reply, answer) = oneshot::channel();
        helper
            .send(Job::Import {
                id,
                fd: fd.try_clone().unwrap(),
                allocation_size: size,
                window: 0..size,
                gpu_uuid: None,
                reply,
            })
            .unwrap();
        assert_eq!(futures::executor::block_on(answer).unwrap(), Ok(ordinal));
        fd
    }

    /// Verify and decrypt `page`'s `range` into request `id` at offset 0.
    pub(crate) fn write(
        helper: &GpuHelper,
        id: u64,
        page: Box<SealedPage>,
        range: Range<u64>,
    ) -> Result<()> {
        let answer = submit(helper, id, page, range)?;
        futures::executor::block_on(answer).unwrap().1
    }

    /// Queue `page`'s `range` for request `id` at offset 0 without waiting.
    pub(super) fn submit(
        helper: &GpuHelper,
        id: u64,
        page: Box<SealedPage>,
        range: Range<u64>,
    ) -> Result<oneshot::Receiver<(Work, Result<()>)>> {
        let ordinal = owner(helper, id).ok_or(Error::Internal)?;
        let (reply, answer) = oneshot::channel();
        helper.submit(
            ordinal,
            WriteTask {
                id,
                offset: 0,
                work: Work::Sealed(Sealed { page, range }),
                reply,
            },
        )?;
        Ok(answer)
    }

    /// The GPU request `id` was imported on.
    pub(crate) fn owner(helper: &GpuHelper, id: u64) -> Option<i32> {
        helper.owners.lock().unwrap().get(&id).copied()
    }

    /// Release request `id` and wait for the answer.
    pub(crate) fn release(helper: &GpuHelper, id: u64) -> Result<()> {
        let (reply, answer) = oneshot::channel();
        helper.release(id, Some(reply))?;
        futures::executor::block_on(answer).unwrap()
    }

    /// Host-to-device copies the GPU has made so far.
    pub(crate) fn host_copies(helper: &GpuHelper) -> usize {
        helper.run(|| {
            simulation()
                .trace()
                .iter()
                .filter(|event| event.operation == racer_gpu::simulation::Operation::CopyH2d)
                .count()
        })
    }

    /// A required-mode helper on simulated GPUs with these NUMA nodes.
    pub(crate) fn helper(numa: &[Option<u32>]) -> Arc<GpuHelper> {
        helper_with(numa, |_| {})
    }

    /// A helper whose simulation `setup` prepares before the backend loads.
    pub(crate) fn helper_with(
        numa: &[Option<u32>],
        setup: impl FnOnce(&racer_gpu::simulation::Simulation) + Send + 'static,
    ) -> Arc<GpuHelper> {
        helper_with_options(numa, GpuOptions::default(), setup)
    }

    /// Like [`helper_with`], with startup options.
    pub(crate) fn helper_with_options(
        numa: &[Option<u32>],
        options: GpuOptions,
        setup: impl FnOnce(&racer_gpu::simulation::Simulation) + Send + 'static,
    ) -> Arc<GpuHelper> {
        let devices: Vec<racer_gpu::simulation::SimDevice> = numa
            .iter()
            .enumerate()
            .map(|(ordinal, node)| {
                let mut device = racer_gpu::simulation::SimDevice::new(ordinal as u8);
                device.numa_node = *node;
                device
            })
            .collect();
        GpuHelper::start_with_options(
            GpuMode::Required,
            options,
            Box::new(move || {
                let simulation = racer_gpu::simulation::Simulation::new(devices);
                setup(&simulation);
                let environment = simulation.enter();
                SIM.with(|sim| *sim.borrow_mut() = Some((simulation, environment)));
                racer_gpu::Gpu::load()
            }),
        )
        .unwrap()
        .unwrap()
    }
}

#[cfg(test)]
mod tests {
    //! RDMA landing buffers on simulated GPUs: allocation, reuse, and
    //! decrypting ciphertext a NIC wrote straight into GPU memory.
    use super::testing::*;
    use super::*;
    use crate::model::*;
    use rdma_verbs::DmabufMemory;

    const LENGTH: u64 = 2 * SLOT_BYTES;

    fn metadata(length: u64) -> ObjectMetadata {
        ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: racer_control_wire::CacheId(
                        "00000000-0000-4000-8000-000000000001".into(),
                    ),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            length,
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        }
    }

    /// A sealed page whose real ciphertext a NIC wrote at `at` in `landing`.
    fn landed(landing: &GpuLanding, at: u64, plaintext: &[u8], tamper: bool) -> Box<SealedPage> {
        let (sealed, page) = seal(plaintext, tamper);
        nic_write(landing, at, &sealed);
        page(SealedSource::Landed(Arc::new(LandedCiphertext::for_test(
            landing,
            at,
            sealed.len() as u64,
        ))))
    }

    /// Seal `plaintext`. Returns the ciphertext and tag, and a builder for
    /// the sealed page given where that ciphertext is.
    fn seal(
        plaintext: &[u8],
        tamper: bool,
    ) -> (Vec<u8>, impl FnOnce(SealedSource) -> Box<SealedPage>) {
        let metadata = metadata(plaintext.len() as u64);
        let envelope = PageEnvelope {
            cipher: PageCipher::Xaes256Gcm,
            page: PageId {
                version: metadata.version.clone(),
                number: PageNumber(0),
            },
            key_id: key_id_from_generation(1, 1).unwrap(),
            nonce: Nonce([2; 24]),
            plaintext_length: plaintext.len() as u32,
            ciphertext_length: plaintext.len() as u32 + 16,
        };
        let aad = crate::security::page_aad(&envelope).unwrap();
        let mut sealed = vec![0; plaintext.len() + 16];
        racer_crypto::seal(&[7; 32], &envelope.nonce.0, &aad, plaintext, &mut sealed).unwrap();
        if tamper {
            sealed[3] ^= 1;
        }
        let build = move |source| {
            let page = Box::new(SealedPage {
                metadata,
                aad,
                key: racer_crypto::xaes_derive(&[7; 32], &envelope.nonce.0),
                source,
                envelope,
            });
            page.validate_for(&page.envelope.page.clone()).unwrap();
            page
        };
        (sealed, build)
    }

    #[test]
    /// One landing buffer per GPU, made once and reused; each GPU gets its
    /// own NIC address range; bad sizes and non-decrypting GPUs are refused.
    fn landing_buffers_are_per_gpu_and_reused() {
        let helper = helper(&[Some(0), Some(1)]);
        let first = landing(&helper, 0, LENGTH).unwrap();
        assert!(Arc::ptr_eq(&first, &landing(&helper, 0, LENGTH).unwrap()));
        let second = landing(&helper, 1, LENGTH).unwrap();
        assert_eq!((first.ordinal(), second.ordinal()), (0, 1));
        assert_eq!((first.offset(), first.length()), (0, LENGTH as usize));
        assert_ne!(first.iova(), second.iova());
        assert_eq!(landing(&helper, 2, LENGTH).err(), Some(Error::Unavailable));
        assert_eq!(landing(&helper, 0, 0).err(), Some(Error::Unavailable));
        assert_eq!(landing(&helper, 0, 4095).err(), Some(Error::Unavailable));
        assert_eq!(helper.run(|| simulation().live().staging), 2);
        assert_eq!(helper.leaked(), 0);
    }

    #[test]
    /// Allocation or export failures are reported and can be retried.
    fn landing_fails_when_allocation_or_export_fails() {
        let helper = helper(&[None]);
        helper.run(|| simulation().fault(racer_gpu::simulation::Operation::Staging, -4));
        assert_eq!(landing(&helper, 0, LENGTH).err(), Some(Error::Overloaded));
        helper.run(|| simulation().fault(racer_gpu::simulation::Operation::Dmabuf, -5));
        assert_eq!(landing(&helper, 0, LENGTH).err(), Some(Error::Unavailable));
        assert!(landing(&helper, 0, LENGTH).is_ok());
    }

    #[test]
    /// Landed ciphertext decrypts on the GPU with no host-to-device copy of
    /// the ciphertext, whole or in part.
    fn landed_ciphertext_decrypts_on_the_gpu_without_a_host_copy() {
        let helper = helper(&[None]);
        let buffer = landing(&helper, 0, LENGTH).unwrap();
        let plaintext: Vec<u8> = (0..5000).map(|i| (i % 251) as u8).collect();
        let fd = import(&helper, 1, 0);
        let before = host_copies(&helper);
        write(
            &helper,
            1,
            landed(&buffer, SLOT_BYTES, &plaintext, false),
            0..5000,
        )
        .unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        assert_eq!(host_copies(&helper), before, "no host copy");
        assert_eq!(helper.counters().landed.load(Ordering::Relaxed), 1);
        let fd = import(&helper, 2, 0);
        write(&helper, 2, landed(&buffer, 0, &plaintext, false), 10..20).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 10),
            plaintext[10..20]
        );
        assert_eq!(helper.counters().landed.load(Ordering::Relaxed), 2);
        // The simulated GPU does not order RDMA writes: each decrypt flushed.
        assert_eq!(helper.run(|| simulation().rdma_flushes()), 2);
        assert_eq!(helper.leaked(), 0);
    }

    #[test]
    /// Landed ciphertext is not read when RDMA writes can be neither ordered
    /// nor flushed; with native ordering no flush is needed.
    fn landed_ciphertext_needs_ordered_or_flushed_rdma_writes() {
        let helper = helper(&[None]);
        let buffer = landing(&helper, 0, LENGTH).unwrap();
        let plaintext: Vec<u8> = (0..5000).map(|i| (i % 251) as u8).collect();
        let _fd = import(&helper, 1, 0);
        helper.run(|| simulation().set_rdma_ordering(0, false));
        let page = landed(&buffer, 0, &plaintext, false);
        assert!(write(&helper, 1, page, 0..5000).is_err());
        assert_eq!(helper.counters().landed.load(Ordering::Relaxed), 0);
        helper.run(|| simulation().set_rdma_ordering(100, false));
        write(&helper, 1, landed(&buffer, 0, &plaintext, false), 0..5000).unwrap();
        assert_eq!(helper.run(|| simulation().rdma_flushes()), 0);
    }

    #[test]
    /// A tampered landed page is rejected before anything is written; a page
    /// on another GPU's landing buffer is copied over and decrypted.
    fn tampered_landed_ciphertext_is_rejected_and_far_landed_is_copied() {
        let helper = helper(&[None, None]);
        let near = landing(&helper, 0, LENGTH).unwrap();
        let far = landing(&helper, 1, LENGTH).unwrap();
        let plaintext = vec![5; 100];
        let fd = import(&helper, 1, 0);
        assert_eq!(
            write(&helper, 1, landed(&near, 0, &plaintext, true), 0..100),
            Err(Error::CorruptRecord)
        );
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 100),
            vec![0; 100]
        );
        assert_eq!(helper.counters().rejected.load(Ordering::Relaxed), 1);
        // A page on another GPU's landing buffer is copied over first.
        write(&helper, 1, landed(&far, 0, &plaintext, false), 0..100).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 100),
            plaintext
        );
        assert_eq!(helper.counters().landed.load(Ordering::Relaxed), 1);
        assert_eq!(helper.counters().peer_copies.load(Ordering::Relaxed), 1);
    }

    /// A memfd standing in for a slab file.
    fn slab(bytes: &[u8]) -> OwnedFd {
        use std::os::unix::fs::FileExt;
        let fd = unsafe { libc::memfd_create(c"slab".as_ptr(), libc::MFD_CLOEXEC) };
        assert!(fd >= 0);
        let file = unsafe { <std::fs::File as std::os::fd::FromRawFd>::from_raw_fd(fd) };
        file.write_all_at(bytes, 0).unwrap();
        file.into()
    }

    /// Stage `length` bytes at `offset` of `fd` on GPU 0, blocking.
    fn stage(
        helper: &GpuHelper,
        fd: &OwnedFd,
        offset: u64,
        length: u64,
    ) -> Result<StagedCiphertext> {
        stage_on(helper, 0, fd, offset, length)
    }

    /// Stage `length` bytes at `offset` of `fd` on GPU `ordinal`, blocking.
    fn stage_on(
        helper: &GpuHelper,
        ordinal: i32,
        fd: &OwnedFd,
        offset: u64,
        length: u64,
    ) -> Result<StagedCiphertext> {
        futures::executor::block_on(helper.stage_file(
            ordinal,
            fd.try_clone().unwrap(),
            offset,
            length,
            &scope(),
        ))
    }

    fn operations(helper: &GpuHelper, operation: racer_gpu::simulation::Operation) -> usize {
        helper.run(move || {
            simulation()
                .trace()
                .iter()
                .filter(|event| event.operation == operation)
                .count()
        })
    }

    #[test]
    /// Many stages in flight at once share one registration, each gets its
    /// own slot, and the slots come back when the answers drop.
    fn concurrent_stages_share_one_file_registration() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None]);
        let bytes: Vec<u8> = (0..(16 * 4096)).map(|i| (i % 253) as u8).collect();
        let fd = slab(&bytes);
        let free = helper.free_slots(0);
        let staged = futures::executor::block_on(futures::future::join_all((0..8).map(|i| {
            let fd = fd.try_clone().unwrap();
            let scope = scope();
            let helper = helper.clone();
            async move { helper.stage_file(0, fd, i * 4096, 4096, &scope).await }
        })));
        let staged: Vec<_> = staged.into_iter().map(Result::unwrap).collect();
        let mut slots: Vec<_> = staged.iter().map(|staged| staged.offset()).collect();
        slots.sort_unstable();
        slots.dedup();
        assert_eq!(slots.len(), 8);
        assert_eq!(helper.free_slots(0), free - 8);
        assert_eq!(helper.counters().staged.load(Ordering::Relaxed), 8);
        assert_eq!(operations(&helper, Operation::FileRead), 8);
        assert_eq!(operations(&helper, Operation::File), 1);
        drop(staged);
        assert_eq!(helper.free_slots(0), free);
        assert_eq!(helper.run(|| simulation().live().files), 1);
    }

    #[test]
    /// A failed read frees its slot and the file is registered again for the
    /// next read; cuFile stays on.
    fn failed_read_registers_the_file_again() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None]);
        let fd = slab(&[7; 8192]);
        let free = helper.free_slots(0);
        stage(&helper, &fd, 0, 4096).unwrap();
        helper.run(|| simulation().fault(Operation::FileRead, -5));
        assert_eq!(stage(&helper, &fd, 0, 4096).err(), Some(Error::Unavailable));
        assert_eq!(helper.free_slots(0), free);
        assert_eq!(helper.counters().file_failures.load(Ordering::Relaxed), 1);
        assert!(helper.cufile());
        stage(&helper, &fd, 4096, 4096).unwrap();
        assert_eq!(operations(&helper, Operation::File), 2);
        assert_eq!(operations(&helper, Operation::FileFree), 1);
        assert_eq!(helper.run(|| simulation().live().files), 1);
        assert_eq!(helper.counters().staged.load(Ordering::Relaxed), 2);
    }

    #[test]
    /// cuFile rejecting one file as unsupported keeps cuFile on, remembers
    /// the file's device so it is not registered again, and leaves files on
    /// other devices on cuFile.
    fn unsupported_file_skips_its_device_only() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None]);
        let fd = slab(&[7; 8192]);
        helper.run(|| simulation().fault(Operation::File, -11));
        assert_eq!(stage(&helper, &fd, 0, 4096).err(), Some(Error::Unavailable));
        assert!(helper.cufile());
        assert_eq!(helper.counters().file_failures.load(Ordering::Relaxed), 1);
        assert_eq!(
            helper.counters().cufile_unsupported.load(Ordering::Relaxed),
            1
        );
        assert_eq!(operations(&helper, Operation::File), 1);
        // Same device: no second registration, still the host path.
        let other = slab(&[8; 4096]);
        assert_eq!(
            stage(&helper, &other, 0, 4096).err(),
            Some(Error::Unavailable)
        );
        assert_eq!(operations(&helper, Operation::File), 1);
        assert_eq!(helper.counters().file_failures.load(Ordering::Relaxed), 1);
        // A file on another device still goes through cuFile.
        let elsewhere: OwnedFd = std::fs::File::open("/proc/self/exe").unwrap().into();
        stage(&helper, &elsewhere, 0, 4096).unwrap();
        assert_eq!(operations(&helper, Operation::File), 2);
        assert_eq!(helper.counters().staged.load(Ordering::Relaxed), 1);
        assert!(helper.cufile());
        // The device is counted once, and cuFile was not turned off.
        assert_eq!(
            helper.counters().cufile_unsupported.load(Ordering::Relaxed),
            1
        );
        assert_eq!(
            helper.counters().cufile_disabled[CufileDisable::FileOpen as usize]
                .load(Ordering::Relaxed),
            0
        );
    }

    #[test]
    /// A driver failure while registering a file still turns cuFile off for
    /// the whole node.
    fn driver_failure_on_register_turns_cufile_off() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None]);
        let fd = slab(&[7; 4096]);
        helper.run(|| simulation().fault(Operation::File, -5));
        assert_eq!(stage(&helper, &fd, 0, 4096).err(), Some(Error::Unavailable));
        assert!(!helper.cufile());
        assert_eq!(helper.counters().file_failures.load(Ordering::Relaxed), 1);
        assert_eq!(
            helper.counters().cufile_disabled[CufileDisable::FileOpen as usize]
                .load(Ordering::Relaxed),
            1
        );
        assert_eq!(
            helper.counters().cufile_unsupported.load(Ordering::Relaxed),
            0
        );
    }

    /// Wait for the helper's background work to reach `done`.
    fn eventually(mut done: impl FnMut() -> bool) {
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(10);
        while !done() {
            assert!(std::time::Instant::now() < deadline, "timed out");
            std::thread::sleep(std::time::Duration::from_millis(1));
        }
    }

    #[test]
    /// The cuFile driver opens at startup, before any sealed page asks for it.
    fn cufile_driver_opens_at_startup() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None]);
        eventually(|| operations(&helper, Operation::FileOpen) == 1);
        assert!(helper.cufile());
        let fd = slab(&[3; 4096]);
        stage(&helper, &fd, 0, 4096).unwrap();
        assert_eq!(helper.counters().staged.load(Ordering::Relaxed), 1);
    }

    /// Default options with cuFile PCI P2PDMA set to `enabled`.
    fn p2pdma(enabled: bool) -> GpuOptions {
        GpuOptions {
            pci_p2pdma: enabled,
            ..GpuOptions::default()
        }
    }

    #[test]
    /// `RACER_GPU_PCI_P2PDMA` reaches the startup cuFile driver open, and is
    /// off by default.
    fn cufile_driver_opens_with_pci_p2pdma_setting() {
        use racer_gpu::simulation::Operation;
        for enabled in [false, true] {
            let helper = helper_with_options(&[None], p2pdma(enabled), |_| {});
            eventually(|| operations(&helper, Operation::FileOpen) == 1);
            assert_eq!(helper.run(|| simulation().pci_p2pdma()), Some(enabled));
            assert!(helper.cufile());
        }
    }

    #[test]
    /// A driver open with PCI P2PDMA that fails turns cuFile off, so pages
    /// use the host path.
    fn cufile_pci_p2pdma_open_failure_turns_cufile_off() {
        use racer_gpu::simulation::Operation;
        let helper = helper_with_options(&[None], p2pdma(true), |simulation| {
            simulation.fault(Operation::FileOpen, -3)
        });
        eventually(|| !helper.cufile());
        assert_eq!(helper.run(|| simulation().pci_p2pdma()), None);
        assert_eq!(helper.counters().staged.load(Ordering::Relaxed), 0);
    }

    #[test]
    /// A driver that fails to open at startup turns cuFile off before any
    /// sealed page asks for it.
    fn cufile_driver_open_failure_turns_cufile_off() {
        use racer_gpu::simulation::Operation;
        let helper = helper_with(&[None], |simulation| {
            simulation.fault(Operation::FileOpen, -5)
        });
        eventually(|| !helper.cufile());
        assert_eq!(helper.counters().staged.load(Ordering::Relaxed), 0);
    }

    #[test]
    /// A short file read fails cleanly, as it would on a truncated slab.
    fn short_read_fails_the_stage() {
        let helper = helper(&[None]);
        let fd = slab(&[1; 4096]);
        assert_eq!(
            stage(&helper, &fd, 2048, 4096).err(),
            Some(Error::Unavailable)
        );
        assert_eq!(helper.counters().file_failures.load(Ordering::Relaxed), 1);
    }

    /// A sealed page whose ciphertext cuFile staged on GPU `ordinal`.
    fn staged(helper: &GpuHelper, ordinal: i32, plaintext: &[u8]) -> Box<SealedPage> {
        let (sealed, page) = seal(plaintext, false);
        let fd = slab(&sealed);
        let staged = stage_on(helper, ordinal, &fd, 0, sealed.len() as u64).unwrap();
        page(SealedSource::Staged {
            staged: Arc::new(staged),
            token: crate::store::ReadToken::for_test(PageId {
                version: metadata(0).version,
                number: PageNumber(0),
            }),
        })
    }

    fn counter(value: &AtomicU64) -> u64 {
        value.load(Ordering::Relaxed)
    }

    fn plaintext() -> Vec<u8> {
        (0..5000).map(|i| (i % 249) as u8).collect()
    }

    #[test]
    /// Staged ciphertext on the request's GPU decrypts straight from the
    /// slot: no peer copy and no host copy.
    fn same_gpu_staged_ciphertext_needs_no_copy() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None, None]);
        assert!(helper.peers(0, 0) && helper.peers(0, 1) && helper.peers(1, 0));
        let plaintext = plaintext();
        let fd = import(&helper, 1, 0);
        let page = staged(&helper, 0, &plaintext);
        let before = host_copies(&helper);
        write(&helper, 1, page, 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        assert_eq!(host_copies(&helper), before);
        assert_eq!(operations(&helper, Operation::CopyPeer), 0);
        assert_eq!(counter(&helper.counters().peer_copies), 0);
        assert_eq!(counter(&helper.counters().peer_bounces), 0);
    }

    #[test]
    /// Staged ciphertext on another GPU is copied over the peer path, then
    /// decrypted on the request's GPU, whole or in part.
    fn cross_gpu_staged_ciphertext_uses_a_peer_copy() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None, None]);
        let plaintext = plaintext();
        let fd = import(&helper, 1, 1);
        let before = host_copies(&helper);
        write(&helper, 1, staged(&helper, 0, &plaintext), 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        assert_eq!(host_copies(&helper), before, "no host copy");
        assert_eq!(operations(&helper, Operation::CopyPeer), 1);
        assert_eq!(counter(&helper.counters().peer_copies), 1);
        assert_eq!(counter(&helper.counters().peer_copy_bytes), 5016);
        let fd = import(&helper, 2, 1);
        write(&helper, 2, staged(&helper, 0, &plaintext), 100..200).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 100),
            plaintext[100..200]
        );
        assert_eq!(counter(&helper.counters().peer_copies), 2);
        assert_eq!(counter(&helper.counters().decrypted), 2);
        assert_eq!(helper.free_slots(0), helper.free_slots(1));
        assert_eq!(helper.leaked(), 0);
        let text = scrape(&helper);
        assert!(text.contains("racer_gpu_peer_copies_total 2\n"), "{text}");
        let bytes = counter(&helper.counters().peer_copy_bytes);
        assert!(bytes > 5016);
        assert!(text.contains(&format!("racer_gpu_peer_copy_bytes_total {bytes}\n")));
        assert!(text.contains("racer_gpu_peer_failures_total 0\n"));
        assert!(text.contains("racer_gpu_peer_bounces_total 0\n"));
        assert!(text.contains("racer_gpu_read_pages_total{decrypt=\"gpu\"} 2\n"));
    }

    /// Render the Prometheus text a worker scrape would produce.
    fn scrape(helper: &Arc<GpuHelper>) -> String {
        let metrics = crate::telemetry::Metrics::for_workers(2).unwrap();
        metrics[0].gpu().attach_helper(helper.clone());
        let mut text = String::new();
        metrics[1].write_prometheus(&mut text).unwrap();
        text
    }

    #[test]
    /// Landed ciphertext on another GPU's landing buffer is copied over the
    /// peer path and decrypted on the request's GPU.
    fn cross_gpu_landed_ciphertext_uses_a_peer_copy() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None, None]);
        let far = landing(&helper, 0, LENGTH).unwrap();
        let plaintext = vec![9; 300];
        let fd = import(&helper, 1, 1);
        write(
            &helper,
            1,
            landed(&far, SLOT_BYTES, &plaintext, false),
            0..300,
        )
        .unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 300),
            plaintext
        );
        assert_eq!(operations(&helper, Operation::CopyPeer), 1);
        assert_eq!(counter(&helper.counters().peer_copy_bytes), 316);
        assert_eq!(counter(&helper.counters().landed), 1);
        // The receiving GPU still checks the tag.
        assert_eq!(
            write(&helper, 1, landed(&far, 0, &plaintext, true), 0..300),
            Err(Error::CorruptRecord)
        );
        assert_eq!(counter(&helper.counters().rejected), 1);
    }

    #[test]
    /// A failed peer copy turns the pair off, the page still arrives through
    /// host memory, and later pages skip the peer path.
    fn failed_peer_copy_disables_the_pair_and_bounces() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None, None]);
        let plaintext = plaintext();
        let fd = import(&helper, 1, 1);
        let page = staged(&helper, 0, &plaintext);
        helper.run(|| simulation().fault(Operation::CopyPeer, -5));
        write(&helper, 1, page, 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        assert!(!helper.peers(1, 0));
        assert!(helper.peers(0, 1), "only the failed direction turns off");
        assert_eq!(counter(&helper.counters().peer_failures), 1);
        assert_eq!(counter(&helper.counters().peer_bounces), 1);
        assert_eq!(counter(&helper.counters().peer_copies), 0);
        let attempts = operations(&helper, Operation::CopyPeer);
        let fd = import(&helper, 2, 1);
        write(&helper, 2, staged(&helper, 0, &plaintext), 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        assert_eq!(operations(&helper, Operation::CopyPeer), attempts);
        assert_eq!(counter(&helper.counters().peer_bounces), 2);
        assert_eq!(counter(&helper.counters().peer_failures), 1);
        let text = scrape(&helper);
        assert!(text.contains("racer_gpu_peer_failures_total 1\n"), "{text}");
        assert!(text.contains("racer_gpu_peer_bounces_total 2\n"));
        assert!(text.contains("racer_gpu_peer_copies_total 0\n"));
    }

    #[test]
    /// A failed peer copy, then a failed bounce, leave the source slot out
    /// of the pool when no fence succeeds afterward: neither the request's
    /// stream nor the source GPU's utility stream synchronizes.
    fn unfenced_failed_copy_quarantines_the_source_slot() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None, None]);
        let plaintext = plaintext();
        let _fd = import(&helper, 1, 1);
        let free = helper.free_slots(0);
        let page = staged(&helper, 0, &plaintext);
        helper.run(|| {
            simulation().fault(Operation::CopyPeer, -5);
            simulation().fault(Operation::CopyD2hAsync, -5);
            simulation().fault(Operation::StreamSync, -5);
            simulation().fault(Operation::StreamSync, -5);
        });
        assert!(write(&helper, 1, page, 0..5000).is_err());
        assert_eq!(counter(&helper.counters().peer_failures), 1);
        assert_eq!(helper.free_slots(0), free - 1, "slot stays out");
    }

    #[test]
    /// The same copy failures return the slot when the request's stream
    /// fences afterward.
    fn fenced_failed_copy_returns_the_source_slot() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None, None]);
        let plaintext = plaintext();
        let _fd = import(&helper, 1, 1);
        let free = helper.free_slots(0);
        let page = staged(&helper, 0, &plaintext);
        helper.run(|| {
            simulation().fault(Operation::CopyPeer, -5);
            simulation().fault(Operation::CopyD2hAsync, -5);
        });
        assert!(write(&helper, 1, page, 0..5000).is_err());
        assert_eq!(helper.free_slots(0), free);
    }

    /// Copies and decrypts that may write a GPU's scratch slots so far.
    fn scratch_work(helper: &GpuHelper) -> usize {
        use racer_gpu::simulation::Operation;
        [Operation::CopyH2d, Operation::CopyPeer, Operation::Decrypt]
            .into_iter()
            .map(|operation| operations(helper, operation))
            .sum()
    }

    /// Fail a cross-GPU write on request 1 (GPU 1) so that its stream is
    /// never fenced: the peer copy and the bounce fail to queue, and the
    /// stream fails to synchronize after them.
    fn taint_gpu_one(helper: &GpuHelper) {
        use racer_gpu::simulation::Operation;
        let page = staged(helper, 0, &plaintext());
        helper.run(|| {
            simulation().fault(Operation::CopyPeer, -5);
            simulation().fault(Operation::CopyD2hAsync, -5);
            simulation().fault(Operation::StreamSync, -5);
        });
        assert_eq!(write(helper, 1, page, 0..5000), Err(Error::Unavailable));
    }

    #[test]
    /// An unfenced failed write blocks later sealed writes on the same GPU,
    /// from other requests too, without touching its scratch, until a fence
    /// succeeds. Then writes succeed again.
    fn unfenced_failed_write_blocks_scratch_until_fenced() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None, None]);
        let plaintext = plaintext();
        let _first = import(&helper, 1, 1);
        let fd = import(&helper, 2, 1);
        taint_gpu_one(&helper);
        // The retried fence fails: refused before any scratch work.
        let page = staged(&helper, 0, &plaintext);
        let before = scratch_work(&helper);
        helper.run(|| simulation().fault(Operation::StreamSync, -5));
        assert_eq!(write(&helper, 2, page, 0..5000), Err(Error::Unavailable));
        assert_eq!(scratch_work(&helper), before, "scratch untouched");
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            vec![0; 5000]
        );
        // The fence succeeds now: the write goes through, partial or whole.
        write(&helper, 2, staged(&helper, 1, &plaintext), 10..20).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 10),
            plaintext[10..20]
        );
        write(&helper, 2, staged(&helper, 0, &plaintext), 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        assert_eq!(helper.leaked(), 0);
    }

    #[test]
    /// A taint on one GPU leaves another GPU's sealed writes alone and
    /// stays on its own GPU.
    fn scratch_taint_is_per_gpu() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None, None]);
        let plaintext = plaintext();
        let _first = import(&helper, 1, 1);
        let other = import(&helper, 2, 0);
        taint_gpu_one(&helper);
        write(&helper, 2, staged(&helper, 0, &plaintext), 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&other, 0, 5000),
            plaintext
        );
        // GPU 1 is still tainted: its next failed fence refuses.
        let _third = import(&helper, 3, 1);
        let page = staged(&helper, 1, &plaintext);
        let before = scratch_work(&helper);
        helper.run(|| simulation().fault(Operation::StreamSync, -5));
        assert_eq!(write(&helper, 3, page, 10..20), Err(Error::Unavailable));
        assert_eq!(scratch_work(&helper), before);
    }

    #[test]
    /// A tainting request may be released while its stream is unfenced: the
    /// stream belongs to the executor, which keeps it, nothing leaks, and
    /// the next fence on it clears the taint and frees it.
    fn released_tainting_request_keeps_its_stream_until_fenced() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None, None]);
        let plaintext = plaintext();
        let _first = import(&helper, 1, 1);
        let fd = import(&helper, 2, 1);
        taint_gpu_one(&helper);
        let streams = helper.executor_streams();
        assert_eq!(helper.run(|| simulation().live().streams), streams);
        assert_eq!(release(&helper, 1), Ok(()));
        assert_eq!(helper.executor_streams(), streams, "tainted stream kept");
        helper.run(|| simulation().fault(Operation::StreamSync, -5));
        let page = staged(&helper, 1, &plaintext);
        assert_eq!(write(&helper, 2, page, 0..5000), Err(Error::Unavailable));
        write(&helper, 2, staged(&helper, 1, &plaintext), 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        // The tainted stream is gone; the write made a fresh one.
        assert_eq!(helper.executor_streams(), streams);
        assert_eq!(
            helper.run(|| simulation().live().streams),
            helper.executor_streams()
        );
        assert_eq!(helper.leaked(), 0);
    }

    #[test]
    /// A write whose fence reports a failure fails, but its stream fences,
    /// so the GPU is not tainted.
    fn fenced_failed_write_does_not_block_scratch() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None]);
        let plaintext = plaintext();
        let fd = import(&helper, 1, 0);
        let page = staged(&helper, 0, &plaintext);
        helper.run(|| simulation().fault(Operation::EventQuery, -5));
        assert_eq!(write(&helper, 1, page, 10..20), Err(Error::Unavailable));
        helper.run(|| simulation().fault(Operation::EventQuery, -5));
        // The fault now lands on the page's own fence, not a taint check.
        let page = staged(&helper, 0, &plaintext);
        assert_eq!(write(&helper, 1, page, 0..5000), Err(Error::Unavailable));
        write(&helper, 1, staged(&helper, 0, &plaintext), 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        assert_eq!(helper.leaked(), 0);
    }

    #[test]
    /// Failures on the verdict read and the fence record fail only their
    /// page; the stream fences, nothing is written, and the next page works.
    fn verdict_and_fence_record_faults_fail_the_page() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None]);
        let plaintext = plaintext();
        let fd = import(&helper, 1, 0);
        // A partial page reaches the import only after its verdict.
        helper.run(|| simulation().fault(Operation::Verdict, -5));
        let page = staged(&helper, 0, &plaintext);
        assert_eq!(write(&helper, 1, page, 0..100), Err(Error::Unavailable));
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 100),
            vec![0; 100]
        );
        helper.run(|| simulation().fault(Operation::EventRecord, -5));
        let page = staged(&helper, 0, &plaintext);
        assert_eq!(write(&helper, 1, page, 0..5000), Err(Error::Unavailable));
        write(&helper, 1, staged(&helper, 0, &plaintext), 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        assert_eq!(counter(&helper.counters().decrypted), 1);
        assert_eq!(helper.leaked(), 0);
    }

    #[test]
    /// `RACER_GPU_PEER=off` never enables peer access; ciphertext on another
    /// GPU moves through host memory.
    fn peer_off_bounces_through_the_host() {
        use racer_gpu::simulation::Operation;
        let options = GpuOptions {
            peer: false,
            ..GpuOptions::default()
        };
        let helper = helper_with_options(&[None, None], options, |_| {});
        assert!(!helper.peers(1, 0) && !helper.peers(0, 1));
        assert!(helper.peers(1, 1));
        assert_eq!(operations(&helper, Operation::Peer), 0);
        let plaintext = plaintext();
        let fd = import(&helper, 1, 1);
        write(&helper, 1, staged(&helper, 0, &plaintext), 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            plaintext
        );
        assert_eq!(operations(&helper, Operation::CopyPeer), 0);
        assert_eq!(counter(&helper.counters().peer_bounces), 1);
        assert_eq!(counter(&helper.counters().peer_failures), 0);
    }

    #[test]
    /// A pair with no peer path is reported unreachable at startup.
    fn unreachable_pair_is_not_peered() {
        let helper = helper_with(&[None, None, None], |simulation| {
            simulation.set_peer(1, 0, false)
        });
        assert!(!helper.peers(1, 0));
        assert!(helper.peers(0, 1) && helper.peers(2, 0) && helper.peers(1, 2));
        assert!(!helper.peers(0, 7), "unknown GPU");
    }

    #[test]
    /// Two sealed pages sharing one staged slot keep it until both drop.
    fn shared_staged_slot_returns_after_the_last_holder() {
        let helper = helper(&[None]);
        let free = helper.free_slots(0);
        let first = staged(&helper, 0, &plaintext());
        let SealedSource::Staged { staged, token } = &first.source else {
            unreachable!()
        };
        let (shared, token) = (staged.clone(), token.clone());
        let second = seal(&plaintext(), false).1(SealedSource::Staged {
            staged: shared,
            token,
        });
        assert_eq!(helper.free_slots(0), free - 1);
        drop(first);
        assert_eq!(helper.free_slots(0), free - 1);
        drop(second);
        assert_eq!(helper.free_slots(0), free);
    }

    /// Wait for every answer and return the results.
    fn answers(answers: Vec<oneshot::Receiver<(Work, Result<()>)>>) -> Vec<Result<()>> {
        answers
            .into_iter()
            .map(|answer| futures::executor::block_on(answer).unwrap().1)
            .collect()
    }

    /// Positions of `operation` in the simulation trace.
    fn positions(helper: &GpuHelper, operation: racer_gpu::simulation::Operation) -> Vec<usize> {
        helper.run(move || {
            simulation()
                .trace()
                .iter()
                .enumerate()
                .filter(|(_, event)| event.operation == operation)
                .map(|(index, _)| index)
                .collect()
        })
    }

    #[test]
    /// One GPU runs pages of several requests at once, each on its own
    /// stream: every decrypt is queued before the first verdict is read.
    /// Partial pages share the scratch pairs; more of them than pairs wait
    /// for one and still complete.
    fn pages_run_concurrently_on_several_streams_of_one_gpu() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None]);
        let plaintext = plaintext();
        let fds: Vec<OwnedFd> = (1..=8).map(|id| import(&helper, id, 0)).collect();
        let streams = STREAMS as u64;
        helper.run(|| simulation().set_query_delay(50));
        let mark = helper.run(|| simulation().trace().len());
        let pending = (1..=streams)
            .map(|id| submit(&helper, id, staged(&helper, 0, &plaintext), 0..5000).unwrap())
            .collect();
        assert!(answers(pending).into_iter().all(|result| result.is_ok()));
        let decrypts: Vec<usize> = positions(&helper, Operation::Decrypt)
            .into_iter()
            .filter(|at| *at >= mark)
            .collect();
        let verdicts: Vec<usize> = positions(&helper, Operation::Verdict)
            .into_iter()
            .filter(|at| *at >= mark)
            .collect();
        assert_eq!(decrypts.len(), STREAMS);
        assert!(
            decrypts[STREAMS - 1] < verdicts[0],
            "decrypts {decrypts:?} verdicts {verdicts:?}"
        );
        for fd in &fds[..STREAMS] {
            assert_eq!(
                racer_gpu::simulation::read_allocation(fd, 0, 5000),
                plaintext
            );
        }
        // Eight partial pages, more than the scratch pairs and streams.
        let pending = (1..=8)
            .map(|id| {
                let range = 100 * id..100 * id + 50;
                submit(&helper, id, staged(&helper, 0, &plaintext), range).unwrap()
            })
            .collect();
        assert!(answers(pending).into_iter().all(|result| result.is_ok()));
        for (id, fd) in (1..=8u64).zip(&fds) {
            let start = 100 * id as usize;
            assert_eq!(
                racer_gpu::simulation::read_allocation(fd, 0, 50),
                plaintext[start..start + 50]
            );
        }
        assert_eq!(counter(&helper.counters().decrypted), 12);
        assert_eq!(helper.queued(0), 0);
        assert!(helper.executor_streams() <= STREAMS);
        assert!(helper.run(|| simulation().take_violations()).is_empty());
        assert_eq!(helper.leaked(), 0);
    }

    #[test]
    /// Two GPUs decrypt each other's staged ciphertext at the same time,
    /// over the peer path, and with peer access off through pinned host
    /// memory, where each destination stream waits on the source GPU's
    /// fence instead of the host.
    fn two_gpus_copy_each_others_ciphertext_concurrently() {
        use racer_gpu::simulation::Operation;
        for peer in [true, false] {
            let options = GpuOptions {
                peer,
                ..GpuOptions::default()
            };
            let helper = helper_with_options(&[None, None], options, |_| {});
            let plaintext = plaintext();
            let fds: Vec<(u64, OwnedFd)> = (1..=4u64)
                .map(|id| (id, import(&helper, id, (id % 2) as i32)))
                .collect();
            helper.run(|| simulation().set_query_delay(10));
            let pending = fds
                .iter()
                .map(|(id, _)| {
                    let source = 1 - (id % 2) as i32;
                    let page = staged(&helper, source, &plaintext);
                    submit(&helper, *id, page, 0..5000).unwrap()
                })
                .collect();
            assert!(answers(pending).into_iter().all(|result| result.is_ok()));
            for (_, fd) in &fds {
                assert_eq!(
                    racer_gpu::simulation::read_allocation(fd, 0, 5000),
                    plaintext
                );
            }
            if peer {
                assert_eq!(counter(&helper.counters().peer_copies), 4);
                assert_eq!(operations(&helper, Operation::StreamWait), 0);
            } else {
                assert_eq!(counter(&helper.counters().peer_bounces), 4);
                assert_eq!(operations(&helper, Operation::StreamWait), 4);
                assert_eq!(operations(&helper, Operation::CopyD2h), 0, "no host wait");
            }
            assert!(helper.run(|| simulation().take_violations()).is_empty());
            assert_eq!(helper.leaked(), 0);
        }
    }

    #[test]
    /// A full executor queue refuses more pages with `Overloaded` and counts
    /// them; once its pages complete it accepts pages again.
    fn full_queue_is_overloaded() {
        let options = GpuOptions {
            queue_depth: 2,
            ..GpuOptions::default()
        };
        let helper = helper_with_options(&[None], options, |_| {});
        let plaintext = plaintext();
        let fds: Vec<OwnedFd> = (1..=3).map(|id| import(&helper, id, 0)).collect();
        helper.run(|| simulation().set_query_delay(u32::MAX));
        let pending: Vec<_> = (1..=2)
            .map(|id| submit(&helper, id, staged(&helper, 0, &plaintext), 0..5000).unwrap())
            .collect();
        let refused = submit(&helper, 3, staged(&helper, 0, &plaintext), 0..5000);
        assert_eq!(refused.err(), Some(Error::Overloaded));
        assert_eq!(counter(&helper.counters().queue_full), 1);
        assert_eq!(helper.queued(0), 2);
        helper.run(|| simulation().set_query_delay(0));
        assert!(answers(pending).into_iter().all(|result| result.is_ok()));
        eventually(|| helper.queued(0) == 0);
        write(&helper, 3, staged(&helper, 0, &plaintext), 0..5000).unwrap();
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fds[2], 0, 5000),
            plaintext
        );
    }

    #[test]
    /// Dropping the helper with pages in flight lets the executors finish
    /// and answer them, then releases every import, stream and event.
    fn shutdown_drains_running_pages() {
        let helper = helper(&[None, None]);
        let plaintext = plaintext();
        let fds: Vec<OwnedFd> = (1..=4u64)
            .map(|id| import(&helper, id, (id % 2) as i32))
            .collect();
        let simulation = helper.run(simulation);
        simulation.set_query_delay(20);
        let pending: Vec<_> = (1..=4u64)
            .map(|id| {
                let page = staged(&helper, (id % 2) as i32, &plaintext);
                submit(&helper, id, page, 10..20).unwrap()
            })
            .collect();
        drop(helper);
        assert!(answers(pending).into_iter().all(|result| result.is_ok()));
        for fd in &fds {
            assert_eq!(
                racer_gpu::simulation::read_allocation(fd, 0, 10),
                plaintext[10..20]
            );
        }
        eventually(|| {
            let live = simulation.live();
            live.imports == 0 && live.streams == 0 && live.events == 0
        });
        assert!(simulation.take_violations().is_empty());
    }

    #[test]
    /// Deferred simulation still catches a source that changes while the
    /// executor's queued decrypt reads it: the page fails instead of
    /// writing unauthenticated bytes, and the violation is reported.
    fn changed_source_under_a_queued_decrypt_is_caught() {
        use racer_gpu::simulation::Operation;
        let helper = helper(&[None]);
        let plaintext = plaintext();
        let fd = import(&helper, 1, 0);
        let page = staged(&helper, 0, &plaintext);
        let SealedSource::Staged { staged, .. } = &page.source else {
            unreachable!()
        };
        let offset = staged.offset();
        let decrypts = operations(&helper, Operation::Decrypt);
        helper.run(|| simulation().set_query_delay(u32::MAX));
        let pending = submit(&helper, 1, page, 0..5000).unwrap();
        eventually(|| operations(&helper, Operation::Decrypt) > decrypts);
        helper.run(move || simulation().poke_staging(0, offset + 7, &[0xff; 4]));
        helper.run(|| simulation().set_query_delay(0));
        assert_eq!(answers(vec![pending]), [Err(Error::Unavailable)]);
        let violations = helper.run(|| simulation().take_violations());
        assert!(!violations.is_empty(), "violation reported");
        assert!(violations[0].contains("changed after work reading it was queued"));
        assert_eq!(
            racer_gpu::simulation::read_allocation(&fd, 0, 5000),
            vec![0; 5000]
        );
    }

    #[test]
    fn record_joined_counts() {
        let helper = helper(&[None]);
        helper.record_joined();
        helper.record_joined();
        assert_eq!(counter(&helper.counters().joined), 2);
    }
}
