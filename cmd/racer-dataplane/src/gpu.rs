//! `gpu-read` backend: one GPU helper thread owns every CUDA object.
//!
//! `racer_gpu` types are `!Send`, so all GPU state lives on a single native
//! helper thread started once per process. I/O threads talk to it through a
//! job channel and get answers on one-shot channels, so they never block on
//! CUDA. The helper may block: it synchronizes the request's stream after each
//! page before it answers, which is what makes a page visible to the client.
//!
//! Each request gets its own stream and import. Jobs for one request run in
//! order, and a request has at most one job in flight, because the client path
//! awaits each page before the next. [`MAX_OPEN`] bounds requests.
//!
//! When a request ends without [`DirectSink::finish`] (for example the client
//! disconnected), dropping its sink queues a release job behind any page copy
//! still running. The helper releases the stream first, which synchronizes it,
//! then the import. `racer_gpu` leaks and counts anything whose release fails,
//! so memory a GPU may still write is never freed.
//!
//! # Sealed pages
//!
//! Pages that are not verified plaintext in memory arrive sealed
//! ([`crate::read::sealed::SealedPage`]) and the GPU authenticates and decrypts
//! them. Each GPU has one staging buffer of [`SLOT_BYTES`] slots, allocated on
//! first use and capped by `RACER_GPU_STAGING_BYTES`:
//!
//! - Slot 0 holds host ciphertext copied in for the current job.
//! - Slot 1 holds plaintext when a range covers only part of a page: the GPU
//!   decrypts the whole page there (the tag covers all of it), checks the
//!   verdict, then copies only the requested bytes into the import.
//! - The other slots hold ciphertext that a page's owning worker read from a
//!   local v2 disk record with cuFile ([`GpuHelper::stage_file`]). A
//!   [`StagedCiphertext`] owns its slot and returns it on drop. When every
//!   slot is taken, the owner reads the record through host memory instead.
//!
//! The helper runs one job at a time and synchronizes before it answers, so the
//! two scratch slots are never shared by two jobs and no staged slot is in use
//! by a stream between jobs, which [`racer_gpu::GpuFile::read`] requires.
//!
//! # RDMA landing
//!
//! When RDMA is active, each rail may get one GPU's landing buffer
//! ([`GpuHelper::landing`]): separate GPU memory, capped by
//! `RACER_GPU_LANDING_BYTES`, exported once as a dma-buf and registered with
//! the rail's NIC as an `rdma_verbs` device pool. A peer's NIC writes a
//! `gpu-read` page's ciphertext straight into it, and the GPU decrypts from
//! there ([`LandedCiphertext`]). The CPU never reads or copies those bytes.
//! Landing buffers are never freed while the process runs, and are leaked on
//! purpose when the helper exits, because a NIC registration may still cover
//! them.
//!
//! At startup the helper probes each GPU with a zero-length decrypt. A GPU
//! whose shim has no decrypt kernels, or whose staging cap cannot hold the two
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

/// Scratch slots each job may use: host ciphertext and partial plaintext.
const SCRATCH_SLOTS: u64 = 2;

/// Offset of the host ciphertext scratch slot.
const CIPHERTEXT_SCRATCH: u64 = 0;

/// Offset of the partial plaintext scratch slot.
const PLAINTEXT_SCRATCH: u64 = SLOT_BYTES;

/// Registered cuFile handles kept per process; more are reopened on demand.
const MAX_FILES: usize = 64;

/// Threads that run cuFile reads. One synchronous read keeps an NVMe drive
/// far from busy, and reads on the helper thread would stall decryption.
const FILE_READERS: usize = 4;

/// A shared ciphertext page the GPU stream holds until its copy completes.
struct HostCiphertext(crate::memory::CiphertextPage);

impl AsRef<[u8]> for HostCiphertext {
    fn as_ref(&self) -> &[u8] {
        self.0.bytes()
    }
}

/// Builds the GPU backend on the helper thread.
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
/// slot until dropped. Only the helper thread touches the bytes.
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

/// One GPU's RDMA landing buffer, shared with `rdma_verbs` as a dma-buf.
/// See the module docs, "RDMA landing".
pub struct GpuLanding {
    /// Export of the whole landing buffer.
    fd: OwnedFd,
    ordinal: i32,
    length: u64,
}

impl GpuLanding {
    /// The GPU the memory is on.
    pub fn ordinal(&self) -> i32 {
        self.ordinal
    }
}

impl rdma_verbs::DmabufMemory for GpuLanding {
    fn fd(&self) -> std::os::fd::BorrowedFd<'_> {
        use std::os::fd::AsFd;
        self.fd.as_fd()
    }

    fn offset(&self) -> u64 {
        0
    }

    fn length(&self) -> usize {
        self.length as usize
    }

    fn iova(&self) -> u64 {
        LANDING_IOVA * (self.ordinal as u64 + 1)
    }
}

/// Peer ciphertext that a NIC wrote into a GPU landing buffer. Holds the
/// RDMA device page, so the slot is not reused until this is dropped.
pub struct LandedCiphertext {
    /// Holds the slot. `None` only for test pages written without a NIC.
    _page: Option<rdma_verbs::DevicePage>,
    ordinal: i32,
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
            ordinal: landing.ordinal,
            offset: page.offset(),
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
            ordinal: landing.ordinal,
            offset,
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

    /// Byte offset in the landing buffer.
    pub(crate) fn offset(&self) -> u64 {
        self.offset
    }
}

/// Counters for GPU decryption, for logs and tests.
#[derive(Debug, Default)]
pub struct GpuCounters {
    /// Sealed pages the GPU verified and wrote.
    pub decrypted: AtomicU64,

    /// Sealed pages the GPU rejected (CRC or tag mismatch).
    pub rejected: AtomicU64,

    /// Pages cuFile read straight into staging.
    pub staged: AtomicU64,

    /// Direct reads that found no free staging slot.
    pub exhausted: AtomicU64,

    /// cuFile reads or registrations that failed.
    pub file_failures: AtomicU64,

    /// Sealed pages decrypted from an RDMA landing buffer.
    pub landed: AtomicU64,
}

/// Per-GPU state shared with I/O threads.
struct DeviceShared {
    ordinal: i32,

    /// The startup decrypt probe passed and staging holds the scratch slots.
    decrypt: bool,

    slots: Arc<SlotPool>,
}

/// A message to the helper thread.
enum Job {
    Import {
        id: u64,
        fd: OwnedFd,
        allocation_size: u64,
        window: Range<u64>,
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
    Write {
        id: u64,
        offset: u64,
        work: Work,
        reply: oneshot::Sender<(Work, Result<()>)>,
    },
    Release {
        id: u64,
        reply: Option<oneshot::Sender<Result<()>>>,
    },
    /// Allocate a GPU's RDMA landing buffer and export it as a dma-buf.
    Landing {
        ordinal: i32,
        length: u64,
        reply: oneshot::Sender<Result<OwnedFd>>,
    },
    #[cfg(test)]
    Run(Box<dyn FnOnce() + Send>),
}

/// Handle to the GPU helper thread. Shared by every worker.
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
    /// Landing buffers made so far, one per GPU at most.
    landings: Mutex<HashMap<i32, Arc<GpuLanding>>>,
}

impl GpuHelper {
    /// Resolve `mode` against hardware. Returns `None` when GPU mode is off.
    pub fn start(mode: GpuMode, staging_bytes: u64) -> Result<Option<Arc<Self>>> {
        match mode {
            GpuMode::Disabled => Ok(None),
            GpuMode::Auto if !cfg!(feature = "cuda") => {
                eprintln!("racer-dataplane: stage=gpu state=disabled reason=not-built");
                Ok(None)
            }
            _ => Self::start_with_staging(mode, staging_bytes, Box::new(racer_gpu::Gpu::load)),
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
        let state = HelperState {
            leaked: leaked.clone(),
            cufile: cufile.clone(),
            counters: counters.clone(),
            kernels_off: kernels_off.clone(),
            slots,
        };
        std::thread::Builder::new()
            .name("racer-gpu".into())
            .spawn(move || helper_main(loader, receiver, ready, state))
            .map_err(|_| Error::Unavailable)?;
        let found = started.recv().map_err(|_| Error::Unavailable)?;
        let (devices, decrypt) = match found {
            Ok((devices, decrypt)) if !devices.is_empty() => (devices, decrypt),
            outcome => {
                let reason = match outcome {
                    Err(error) => error.to_string(),
                    Ok(_) => "no-usable-gpu".into(),
                };
                eprintln!("racer-dataplane: stage=gpu state=disabled reason={reason}");
                return if mode == GpuMode::Required {
                    Err(Error::Unavailable)
                } else {
                    Ok(None)
                };
            }
        };
        // Two scratch slots per job; the rest hold cuFile reads.
        let decrypting = slots >= SCRATCH_SLOTS;
        let shared = devices
            .iter()
            .zip(&decrypt)
            .map(|(device, kernels)| DeviceShared {
                ordinal: device.ordinal,
                decrypt: *kernels && decrypting,
                slots: Arc::new(SlotPool {
                    free: Mutex::new((SCRATCH_SLOTS..slots.max(SCRATCH_SLOTS)).rev().collect()),
                }),
            })
            .collect::<Vec<_>>();
        eprintln!(
            "racer-dataplane: stage=gpu state=enabled usable={} decrypt={} staging_slots={}",
            devices.len(),
            shared.iter().filter(|device| device.decrypt).count(),
            slots,
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
        })))
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
        // On cancellation the helper still runs the read; the staged slot
        // then drops with the unread answer and returns to the pool.
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
            .and_then(|l| l.get(&ordinal).cloned())
        {
            return Ok(landing);
        }
        let (reply, mut receiver) = oneshot::channel();
        self.send(Job::Landing {
            ordinal,
            length,
            reply,
        })?;
        let fd = answer(&mut receiver, scope).await??;
        let mut landings = self.landings.lock().map_err(|_| Error::Internal)?;
        Ok(landings
            .entry(ordinal)
            .or_insert_with(|| {
                Arc::new(GpuLanding {
                    fd,
                    ordinal,
                    length,
                })
            })
            .clone())
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

    /// Run `f` on the helper thread and wait for it. Tests use this to reach
    /// thread-local simulation state.
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
            reply,
        })?;
        sink.import = Some(answer);
        Ok(Box::new(sink))
    }
}

/// One `gpu-read` request's view of its import on the helper.
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

/// Wait for a helper answer, or stop early when the request is canceled.
/// Abandoning the wait never abandons the job: the helper still runs it.
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
            self.helper.send(Job::Write {
                id: self.id,
                offset: destination_offset,
                work,
                reply,
            })?;
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
            self.helper.send(Job::Release {
                id: self.id,
                reply: Some(reply),
            })?;
            self.released = true;
            answer(&mut receiver, scope).await?
        })
    }
}

impl Drop for GpuSink {
    fn drop(&mut self) {
        if !self.released {
            // Queued behind any copy still running; the helper syncs first.
            let _ = self.helper.send(Job::Release {
                id: self.id,
                reply: None,
            });
        }
        self.helper.open.fetch_sub(1, Ordering::AcqRel);
    }
}

/// One request's GPU objects. Field order matters: the stream drops first,
/// which synchronizes it, then the import.
struct Request {
    stream: racer_gpu::Stream,
    import: racer_gpu::ImportedAllocation,
}

/// Shared handles the helper thread starts with.
struct HelperState {
    leaked: Arc<AtomicU64>,
    cufile: Arc<AtomicBool>,
    counters: Arc<GpuCounters>,
    /// GPUs whose decrypt kernels failed after startup.
    kernels_off: Arc<Mutex<std::collections::HashSet<i32>>>,
    /// Staging slots per GPU.
    slots: u64,
}

/// A registered cuFile handle and the reads using it.
struct OpenFile {
    file: racer_gpu::GpuFile,
    /// Reads queued or running on a reader thread.
    reads: usize,
    /// No longer in [`Helper::files`]: free it once `reads` is zero.
    retired: bool,
}

/// A cuFile read for a reader thread.
struct FileJob {
    read: racer_gpu::FileRead,
    file: u64,
    staged: StagedCiphertext,
    reply: oneshot::Sender<Result<StagedCiphertext>>,
}

/// A finished cuFile read.
struct FileDone {
    file: u64,
    failed: bool,
}

/// Run cuFile reads until the helper stops sending them. Reports each read
/// to the helper before answering, so a later stage sees a failed handle gone.
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
            eprintln!("racer-dataplane: stage=gpu state=cufile-read-failed reason={error}");
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

/// State owned by the helper thread.
struct Helper {
    gpu: racer_gpu::Gpu,
    contexts: Vec<racer_gpu::Context>,
    /// Each GPU's staging buffer, allocated on first sealed page.
    staging: HashMap<i32, racer_gpu::StagingBuffer>,
    /// Registered files by (device, inode). The slab descriptors are few and
    /// long lived, so one registration serves many reads.
    files: HashMap<(u64, u64), u64>,
    /// Registered files by id, including retired ones that reads still use.
    open: HashMap<u64, OpenFile>,
    next_file: u64,
    /// Reads for the reader threads. `None` once they are told to stop.
    reads: Option<mpsc::Sender<FileJob>>,
    /// Reads the reader threads finished, so their files can be released.
    done: mpsc::Receiver<FileDone>,
    readers: Vec<std::thread::JoinHandle<()>>,
    requests: HashMap<u64, Request>,
    /// RDMA landing buffers by GPU. Never freed: a NIC may hold a
    /// registration over them.
    landing: HashMap<i32, racer_gpu::StagingBuffer>,
    state: HelperState,
}

type Started = racer_gpu::Result<(Vec<DeviceInfo>, Vec<bool>)>;

fn helper_main(
    loader: Loader,
    jobs: mpsc::Receiver<Job>,
    ready: mpsc::SyncSender<Started>,
    state: HelperState,
) {
    let mut helper = match discover(loader) {
        Ok((gpu, contexts, usable)) => {
            let decrypt = contexts.iter().map(probe_decrypt).collect();
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
            let _ = ready.send(Ok((usable, decrypt)));
            Helper {
                gpu,
                contexts,
                staging: HashMap::new(),
                files: HashMap::new(),
                open: HashMap::new(),
                next_file: 0,
                reads: (!readers.is_empty()).then_some(reads),
                done,
                readers,
                requests: HashMap::new(),
                landing: HashMap::new(),
                state,
            }
        }
        Err(error) => {
            let _ = ready.send(Err(error));
            return;
        }
    };
    drop(ready);
    while let Ok(job) = jobs.recv() {
        helper.finish_reads();
        helper.run(job);
    }
    // Every sender is gone: let queued reads finish, then release their files.
    helper.reads = None;
    for reader in std::mem::take(&mut helper.readers) {
        let _ = reader.join();
    }
    helper.finish_reads();
    // Release what is left, stream first.
    let ids: Vec<u64> = helper.requests.keys().copied().collect();
    for id in ids {
        let _ = helper.release(id);
    }
    // A NIC registration may still cover landing memory: never free it.
    for (_, buffer) in helper.landing.drain() {
        std::mem::forget(buffer);
    }
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
        expected_crc: None,
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
            "racer-dataplane: stage=gpu ordinal={} decrypt=unavailable reason={:?}",
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
            "racer-dataplane: stage=gpu ordinal={} uuid={} pci_bus_id={} numa_node={} compute_mode={:?} usable={}",
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
                "racer-dataplane: stage=gpu ordinal={} state=skipped reason={error}",
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

impl Helper {
    fn run(&mut self, job: Job) {
        match job {
            Job::Import {
                id,
                fd,
                allocation_size,
                window,
                reply,
            } => {
                let result = self.import(id, fd, allocation_size, window);
                self.note_leaks();
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
                self.note_leaks();
                // A dropped receiver drops the slot, which frees it.
                if let Err((error, reply)) = result {
                    let _ = reply.send(Err(error));
                }
            }
            Job::Write {
                id,
                offset,
                work,
                reply,
            } => {
                let result = self.write(id, offset, &work);
                self.note_leaks();
                // If the I/O side stopped waiting, the page drops here. Its
                // storage is shared and thread safe, so that is fine.
                let _ = reply.send((work, result));
            }
            Job::Release { id, reply } => {
                let result = self.release(id);
                if let Some(reply) = reply {
                    let _ = reply.send(result);
                }
            }
            Job::Landing {
                ordinal,
                length,
                reply,
            } => {
                let result = self.landing(ordinal, length);
                self.note_leaks();
                let _ = reply.send(result);
            }
            #[cfg(test)]
            Job::Run(f) => f(),
        }
    }

    /// Allocate GPU `ordinal`'s landing buffer, once, and export it.
    fn landing(&mut self, ordinal: i32, length: u64) -> Result<OwnedFd> {
        if !self.landing.contains_key(&ordinal) {
            let context = self
                .contexts
                .iter()
                .find(|context| context.ordinal() == ordinal)
                .ok_or(Error::Unavailable)?;
            let buffer = context.staging(length).map_err(|error| {
                eprintln!(
                    "racer-dataplane: stage=gpu ordinal={ordinal} state=landing-failed reason={error}"
                );
                Error::Overloaded
            })?;
            self.landing.insert(ordinal, buffer);
        }
        let buffer = self.landing.get(&ordinal).ok_or(Error::Internal)?;
        if buffer.size() != length {
            return Err(Error::InvalidConfiguration);
        }
        buffer.dma_buf(0, length).map_err(|error| {
            eprintln!(
                "racer-dataplane: stage=gpu ordinal={ordinal} state=landing-export-failed reason={error}"
            );
            Error::Unavailable
        })
    }

    /// Try each usable GPU until one owns the allocation.
    fn import(&mut self, id: u64, fd: OwnedFd, size: u64, window: Range<u64>) -> Result<i32> {
        let mut last = racer_gpu::Error::WrongDevice;
        for context in &self.contexts {
            let attempt = fd.try_clone().map_err(|_| Error::Unavailable)?;
            match context.import(attempt, size, window.clone()) {
                Ok(import) => {
                    let stream = context.stream().map_err(import_error)?;
                    self.requests.insert(id, Request { stream, import });
                    return Ok(context.ordinal());
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

    fn write(&mut self, id: u64, offset: u64, work: &Work) -> Result<()> {
        let plaintext = match work {
            Work::Plaintext(plaintext) => plaintext,
            Work::Sealed(sealed) => return self.write_sealed(id, offset, sealed),
        };
        let request = self.requests.get(&id).ok_or(Error::Internal)?;
        let destination = racer_gpu::Destination::Import(&request.import);
        let queued = request
            .stream
            .copy_from_host(destination, offset, plaintext.clone());
        // Synchronize even after a failed queue call: part of it may run.
        let synchronized = request.stream.synchronize();
        queued.and(synchronized).map_err(gpu_error)
    }

    /// This GPU's staging buffer, allocated on first use.
    fn staging(&mut self, ordinal: i32) -> Result<&racer_gpu::StagingBuffer> {
        if !self.staging.contains_key(&ordinal) {
            let context = self
                .contexts
                .iter()
                .find(|context| context.ordinal() == ordinal)
                .ok_or(Error::Internal)?;
            let size = self.state.slots.max(SCRATCH_SLOTS) * SLOT_BYTES;
            let buffer = context.staging(size).map_err(|error| {
                eprintln!(
                    "racer-dataplane: stage=gpu ordinal={ordinal} state=staging-failed reason={error}"
                );
                Error::Overloaded
            })?;
            self.staging.insert(ordinal, buffer);
        }
        self.staging.get(&ordinal).ok_or(Error::Internal)
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
        if let Err(error) = self.staging(staged.ordinal) {
            return Err((error, reply));
        }
        let Some(staging) = self.staging.get(&staged.ordinal) else {
            return Err((Error::Internal, reply));
        };
        if let Err(error) = staging.register_cufile() {
            self.state
                .counters
                .file_failures
                .fetch_add(1, Ordering::Relaxed);
            self.disable_cufile(error);
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
                match self.gpu.open_file(fd) {
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
                        self.state
                            .counters
                            .file_failures
                            .fetch_add(1, Ordering::Relaxed);
                        self.disable_cufile(error);
                        return Err((Error::Unavailable, reply));
                    }
                }
            }
        };
        let (Some(open), Some(staging)) =
            (self.open.get_mut(&id), self.staging.get(&staged.ordinal))
        else {
            return Err((Error::Internal, reply));
        };
        // SAFETY: the file stays in `open` while `reads` is above zero, the
        // staging buffer and backend live until the readers are joined, and
        // the slot is this job's alone until the read answers.
        let read = match unsafe {
            open.file
                .detach(staging, staged.offset(), file_offset, length)
        } {
            Ok(read) => read,
            Err(error) => return Err((gpu_error(error), reply)),
        };
        open.reads += 1;
        let job = FileJob {
            read,
            file: id,
            staged,
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

    /// Stop trying cuFile after a failure that is not about one read.
    fn disable_cufile(&self, error: racer_gpu::Error) {
        if self.state.cufile.swap(false, Ordering::Relaxed) {
            eprintln!("racer-dataplane: stage=gpu state=cufile-disabled reason={error}");
        }
    }

    /// Verify and decrypt a sealed page, then write only the requested range.
    fn write_sealed(&mut self, id: u64, offset: u64, sealed: &Sealed) -> Result<()> {
        let ordinal = self
            .requests
            .get(&id)
            .ok_or(Error::Internal)?
            .import
            .ordinal();
        self.staging(ordinal)?;
        let staging = self.staging.get(&ordinal).ok_or(Error::Internal)?;
        let request = self.requests.get(&id).ok_or(Error::Internal)?;
        let page = &sealed.page;
        let length = u64::from(page.plaintext_length());
        if sealed.range.start >= sealed.range.end || sealed.range.end > length {
            return Err(Error::InvalidRange);
        }
        let stream = &request.stream;
        let (source_buffer, source) = match &page.source {
            SealedSource::Host(copy) => {
                let bytes = copy.copy.ciphertext.bytes();
                if bytes.len() as u64 != length + 16 {
                    return Err(Error::CorruptRecord);
                }
                let copied = stream.copy_from_host(
                    racer_gpu::Destination::Staging(staging),
                    CIPHERTEXT_SCRATCH,
                    HostCiphertext(copy.copy.ciphertext.clone()),
                );
                if let Err(error) = copied {
                    let _ = stream.synchronize();
                    return Err(gpu_error(error));
                }
                (staging, CIPHERTEXT_SCRATCH)
            }
            SealedSource::Staged { staged, .. } => {
                if staged.ordinal() != ordinal {
                    return Err(Error::Internal);
                }
                (staging, staged.offset())
            }
            SealedSource::Landed(landed) => {
                if landed.ordinal() != ordinal || landed.len() != length + 16 {
                    return Err(Error::Internal);
                }
                let buffer = self.landing.get(&ordinal).ok_or(Error::Internal)?;
                (buffer, landed.offset())
            }
        };
        let request_key = racer_gpu::DecryptRequest {
            key: page.key.key(),
            nonce: page.key.nonce(),
            aad: &page.aad,
            length,
            expected_crc: page.expected_crc,
        };
        // A whole page decrypts straight into the import. Part of a page
        // decrypts into scratch first, because the tag covers the whole page
        // and nothing outside the window may be written.
        let whole = sealed.range.start == 0 && sealed.range.end == length;
        let (destination, at) = if whole {
            (racer_gpu::Destination::Import(&request.import), offset)
        } else {
            (racer_gpu::Destination::Staging(staging), PLAINTEXT_SCRATCH)
        };
        let queued = stream.decrypt(&request_key, source_buffer, source, destination, at);
        let synchronized = stream.synchronize();
        if let Err(racer_gpu::Error::KernelUnavailable) = queued {
            if let Ok(mut off) = self.state.kernels_off.lock() {
                off.insert(ordinal);
            }
            return Err(Error::Unavailable);
        }
        queued.and(synchronized).map_err(gpu_error)?;
        match stream.verdict().map_err(gpu_error)? {
            racer_gpu::Verdict::Passed { .. } => {}
            racer_gpu::Verdict::CrcMismatch { .. } | racer_gpu::Verdict::TagMismatch { .. } => {
                self.state.counters.rejected.fetch_add(1, Ordering::Relaxed);
                return Err(Error::CorruptRecord);
            }
            racer_gpu::Verdict::NotRun => return Err(Error::Unavailable),
        }
        if !whole {
            let copied = stream.copy_from_staging(
                racer_gpu::Destination::Import(&request.import),
                offset,
                staging,
                PLAINTEXT_SCRATCH + sealed.range.start,
                sealed.range.end - sealed.range.start,
            );
            let synchronized = stream.synchronize();
            copied.and(synchronized).map_err(gpu_error)?;
        }
        self.state
            .counters
            .decrypted
            .fetch_add(1, Ordering::Relaxed);
        if matches!(page.source, SealedSource::Landed(_)) {
            self.state.counters.landed.fetch_add(1, Ordering::Relaxed);
        }
        Ok(())
    }

    /// Drop the stream (which synchronizes it), then the import.
    fn release(&mut self, id: u64) -> Result<()> {
        let Some(request) = self.requests.remove(&id) else {
            return Ok(());
        };
        let before = self.gpu.leaked();
        let synchronized = request.stream.synchronize();
        let Request { stream, import } = request;
        drop(stream);
        drop(import);
        self.note_leaks();
        if synchronized.is_err() || self.gpu.leaked() != before {
            return Err(Error::Unavailable);
        }
        Ok(())
    }

    fn note_leaks(&self) {
        let leaked = self.gpu.leaked();
        if self.state.leaked.swap(leaked, Ordering::Relaxed) != leaked {
            eprintln!("racer-dataplane: stage=gpu state=leaked total={leaked}");
        }
    }
}

#[cfg(test)]
pub(crate) mod testing {
    //! A GPU helper on simulated GPUs, for tests outside the client socket.
    use super::*;
    use std::cell::RefCell;

    thread_local! {
        /// The simulation, on the GPU helper thread only.
        static SIM: RefCell<Option<(racer_gpu::simulation::Simulation, racer_gpu::simulation::Environment)>> =
            const { RefCell::new(None) };
    }

    /// The helper thread's simulation. Call through [`GpuHelper::run`].
    pub(crate) fn simulation() -> racer_gpu::simulation::Simulation {
        SIM.with(|sim| sim.borrow().as_ref().unwrap().0.clone())
    }

    /// A test scope with a few seconds on the real clock.
    pub(crate) fn scope() -> RequestScope {
        RequestScope::new(
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
        let file = std::fs::File::from(landing.fd.try_clone().unwrap());
        file.write_all_at(bytes, at).unwrap();
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
        let (reply, answer) = oneshot::channel();
        helper
            .send(Job::Write {
                id,
                offset: 0,
                work: Work::Sealed(Sealed { page, range }),
                reply,
            })
            .unwrap();
        futures::executor::block_on(answer).unwrap().1
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
        let devices: Vec<racer_gpu::simulation::SimDevice> = numa
            .iter()
            .enumerate()
            .map(|(ordinal, node)| {
                let mut device = racer_gpu::simulation::SimDevice::new(ordinal as u8);
                device.numa_node = *node;
                device
            })
            .collect();
        GpuHelper::start_with(
            GpuMode::Required,
            Box::new(move || {
                let simulation = racer_gpu::simulation::Simulation::new(devices);
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
        nic_write(landing, at, &sealed);
        let page = Box::new(SealedPage {
            metadata,
            aad,
            key: racer_crypto::xaes_derive(&[7; 32], &envelope.nonce.0),
            expected_crc: None,
            source: SealedSource::Landed(LandedCiphertext::for_test(
                landing,
                at,
                sealed.len() as u64,
            )),
            envelope,
        });
        page.validate_for(&page.envelope.page.clone()).unwrap();
        page
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
        assert_eq!(helper.leaked(), 0);
    }

    #[test]
    /// A tampered landed page is rejected before anything is written; a page
    /// on another GPU's landing buffer is refused.
    fn tampered_or_misplaced_landed_ciphertext_is_rejected() {
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
        assert_eq!(
            write(&helper, 1, landed(&far, 0, &plaintext, false), 0..100),
            Err(Error::Internal)
        );
        assert_eq!(helper.counters().landed.load(Ordering::Relaxed), 0);
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
        futures::executor::block_on(helper.stage_file(
            0,
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
}
