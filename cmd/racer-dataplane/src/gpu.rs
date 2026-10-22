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
//! Ciphertext pages (later phase) slot in as another [`Work`] variant: the
//! helper copies the sealed page into a per-GPU staging buffer and queues
//! [`racer_gpu::Stream::decrypt`] into the same import, then synchronizes and
//! checks the verdict before answering.

use crate::client::direct::Destination;
use crate::client::direct::DirectPage;
use crate::client::direct::DirectSink;
use crate::client::direct::GpuService;
use crate::config::GpuMode;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::VerifiedPage;
use crate::runtime::RequestScope;
use futures::channel::oneshot;
use racer_gpu::DeviceInfo;
use std::collections::HashMap;
use std::ops::Range;
use std::os::fd::OwnedFd;
use std::sync::Arc;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::sync::mpsc;
use std::task::Poll;

/// Concurrent `gpu-read` requests per process. More get 503.
pub const MAX_OPEN: usize = 256;

/// Builds the GPU backend on the helper thread.
pub type Loader = Box<dyn FnOnce() -> racer_gpu::Result<racer_gpu::Gpu> + Send>;

/// Plaintext bytes handed to the helper. The page is immutable and its
/// storage is reference counted, so the helper reads it in place; the handle
/// goes back with the answer so the page is released on the I/O thread.
struct Plaintext {
    page: VerifiedPage,
    range: Range<usize>,
}

impl Plaintext {
    fn bytes(&self) -> &[u8] {
        &self.page.bytes()[self.range.clone()]
    }
}

/// Work for one request on the GPU.
enum Work {
    /// Copy verified plaintext from host memory into the import.
    Plaintext(Plaintext),
}

/// A message to the helper thread.
enum Job {
    Import {
        id: u64,
        fd: OwnedFd,
        allocation_size: u64,
        window: Range<u64>,
        reply: oneshot::Sender<Result<()>>,
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
}

impl GpuHelper {
    /// Resolve `mode` against hardware. Returns `None` when GPU mode is off.
    pub fn start(mode: GpuMode) -> Result<Option<Arc<Self>>> {
        match mode {
            GpuMode::Disabled => Ok(None),
            GpuMode::Auto if !cfg!(feature = "cuda") => {
                eprintln!("racer-dataplane: stage=gpu state=disabled reason=not-built");
                Ok(None)
            }
            _ => Self::start_with(mode, Box::new(racer_gpu::Gpu::load)),
        }
    }

    /// Start the helper with a custom backend loader.
    pub fn start_with(mode: GpuMode, loader: Loader) -> Result<Option<Arc<Self>>> {
        if mode == GpuMode::Disabled {
            return Ok(None);
        }
        let (jobs, receiver) = mpsc::channel();
        let (ready, started) = mpsc::sync_channel(1);
        let leaked = Arc::new(AtomicU64::new(0));
        let helper_leaked = leaked.clone();
        std::thread::Builder::new()
            .name("racer-gpu".into())
            .spawn(move || helper_main(loader, receiver, ready, helper_leaked))
            .map_err(|_| Error::Unavailable)?;
        let found = started.recv().map_err(|_| Error::Unavailable)?;
        let devices = match found {
            Ok(devices) if !devices.is_empty() => devices,
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
        eprintln!(
            "racer-dataplane: stage=gpu state=enabled usable={}",
            devices.len()
        );
        Ok(Some(Arc::new(Self {
            jobs,
            devices,
            next: AtomicU64::new(1),
            open: AtomicUsize::new(0),
            leaked,
        })))
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
    import: Option<oneshot::Receiver<Result<()>>>,
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
            result?;
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

/// State owned by the helper thread.
struct Helper {
    gpu: racer_gpu::Gpu,
    contexts: Vec<racer_gpu::Context>,
    requests: HashMap<u64, Request>,
    leaked: Arc<AtomicU64>,
}

fn helper_main(
    loader: Loader,
    jobs: mpsc::Receiver<Job>,
    ready: mpsc::SyncSender<racer_gpu::Result<Vec<DeviceInfo>>>,
    leaked: Arc<AtomicU64>,
) {
    let mut helper = match discover(loader) {
        Ok((gpu, contexts, usable)) => {
            let _ = ready.send(Ok(usable));
            Helper {
                gpu,
                contexts,
                requests: HashMap::new(),
                leaked,
            }
        }
        Err(error) => {
            let _ = ready.send(Err(error));
            return;
        }
    };
    drop(ready);
    while let Ok(job) = jobs.recv() {
        helper.run(job);
    }
    // Every sender is gone: release what is left, stream first.
    let ids: Vec<u64> = helper.requests.keys().copied().collect();
    for id in ids {
        let _ = helper.release(id);
    }
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
            #[cfg(test)]
            Job::Run(f) => f(),
        }
    }

    /// Try each usable GPU until one owns the allocation.
    fn import(&mut self, id: u64, fd: OwnedFd, size: u64, window: Range<u64>) -> Result<()> {
        let mut last = racer_gpu::Error::WrongDevice;
        for context in &self.contexts {
            let attempt = fd.try_clone().map_err(|_| Error::Unavailable)?;
            match context.import(attempt, size, window.clone()) {
                Ok(import) => {
                    let stream = context.stream().map_err(import_error)?;
                    self.requests.insert(id, Request { stream, import });
                    return Ok(());
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
        let request = self.requests.get(&id).ok_or(Error::Internal)?;
        let destination = racer_gpu::Destination::Import(&request.import);
        let queued = match work {
            Work::Plaintext(plaintext) => {
                request
                    .stream
                    .copy_from_host(destination, offset, plaintext.bytes())
            }
        };
        // Synchronize even after a failed queue call: part of it may run.
        let synchronized = request.stream.synchronize();
        queued.and(synchronized).map_err(|error| match error {
            racer_gpu::Error::InvalidRange => Error::InvalidRange,
            _ => Error::Unavailable,
        })
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
        if self.leaked.swap(leaked, Ordering::Relaxed) != leaked {
            eprintln!("racer-dataplane: stage=gpu state=leaked total={leaked}");
        }
    }
}

#[cfg(test)]
mod tests;
