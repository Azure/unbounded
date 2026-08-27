//! Control-thread resource ownership, runtime lifecycle, and reconciliation.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{Receiver, RecvTimeoutError, Sender, channel};
use std::sync::{Arc, Mutex, Weak};
use std::time::Duration;

use super::io::{DiskInner, ExportInner};
use super::limits::Limits;
use super::sys;
use super::ublk;
use super::worker::{Ack, Ctl, Doorbell};
use super::{Disk, Export, Handler, Limiter, QUEUES_PER_WORKER, limits};
use crate::kernel;

/// `Counter::Running` makes a second [`start`] fail, so runtime tests queue up.
#[cfg(test)]
pub(crate) fn exclusive() -> std::sync::MutexGuard<'static, ()> {
    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    LOCK.lock().unwrap_or_else(|p| p.into_inner())
}

struct ThreadCtx<C> {
    resources: ResourceBuild,
    hub: Hub<C>,
    current: Option<Arc<C>>,
    retiring: Vec<Arc<C>>,
}

type Job<C> = Box<dyn FnOnce(&mut ThreadCtx<C>) + Send>;

struct Inner<C> {
    tx: Mutex<Option<Sender<Job<C>>>>,
    control: Mutex<Option<kernel::Thread>>,
    workers: Mutex<Vec<kernel::Thread>>,
    down: AtomicBool,
}

pub struct Runtime<C> {
    inner: Arc<Inner<C>>,
}

impl<C> Clone for Runtime<C> {
    fn clone(&self) -> Self {
        Runtime {
            inner: self.inner.clone(),
        }
    }
}

impl<C: Sync + 'static> Runtime<C> {
    /// Install a new configuration, blocking until every core has cut over.
    pub fn update<F>(&self, build: F) -> Result<bool, UpdateError>
    where
        F: FnOnce(&ResourceBuild, Option<&C>) -> std::io::Result<Option<C>> + Send + 'static,
    {
        if self.inner.down.load(Ordering::Acquire) {
            return Err(UpdateError::Runtime(std::io::Error::from_raw_os_error(
                libc::EINVAL,
            )));
        }
        let (tx, rx) = channel();
        post(
            &self.inner,
            Box::new(move |ctx| {
                let _ = tx.send(reconcile(ctx, build));
            }),
        )
        .map_err(UpdateError::Runtime)?;
        kernel::recv(&rx)
            .map_err(|_| UpdateError::Runtime(std::io::Error::from_raw_os_error(libc::EPIPE)))?
    }

    /// Stop serving, tear down every device, wait for workers. Idempotent; runs on drop.
    pub fn shutdown(&self) -> std::io::Result<()> {
        stop(&self.inner)
    }

    #[cfg(test)]
    pub(super) fn request(
        &self,
        core: usize,
        req: super::Request,
    ) -> std::io::Result<Result<(), super::Errno>> {
        let (tx, rx) = channel();
        post(
            &self.inner,
            Box::new(move |ctx| {
                let _ = ctx.hub.request(core, req, tx);
            }),
        )?;
        kernel::recv(&rx).map_err(|_| std::io::Error::from_raw_os_error(libc::EPIPE))
    }
}

impl<C> Drop for Runtime<C> {
    fn drop(&mut self) {
        if Arc::strong_count(&self.inner) == 1 {
            let _ = stop(&self.inner);
        }
    }
}

fn post<C>(inner: &Inner<C>, job: Job<C>) -> std::io::Result<()> {
    let tx = inner.tx.lock().unwrap();
    match tx.as_ref() {
        Some(tx) => tx
            .send(job)
            .map_err(|_| std::io::Error::from_raw_os_error(libc::EPIPE)),
        None => Err(std::io::Error::from_raw_os_error(libc::EINVAL)),
    }
}

fn stop<C>(inner: &Inner<C>) -> std::io::Result<()> {
    if inner.down.swap(true, Ordering::AcqRel) {
        return Ok(());
    }
    let (tx, rx) = channel();
    let posted = post(
        inner,
        Box::new(move |ctx| {
            let _ = tx.send(teardown(ctx));
        }),
    );
    let r = match posted {
        Ok(()) => kernel::recv(&rx)
            .unwrap_or_else(|_| Err(std::io::Error::from_raw_os_error(libc::EPIPE))),
        Err(e) => Err(e),
    };
    // Dropping the sender ends the control loop.
    *inner.tx.lock().unwrap() = None;
    if let Some(h) = inner.control.lock().unwrap().take() {
        let _ = h.join();
    }
    for h in inner.workers.lock().unwrap().drain(..) {
        let _ = h.join();
    }
    kernel::set_counter(kernel::Counter::Running, 0);
    r
}

/// Boot the runtime.
pub fn start<H: Handler>() -> std::io::Result<Runtime<H::Config>> {
    if kernel::swap_counter(kernel::Counter::Running, 1) != 0 {
        return Err(std::io::Error::from_raw_os_error(libc::EEXIST));
    }
    match boot::<H>() {
        Ok(rt) => Ok(rt),
        Err(e) => {
            kernel::set_counter(kernel::Counter::Running, 0);
            Err(e)
        }
    }
}

fn boot<H: Handler>() -> std::io::Result<Runtime<H::Config>> {
    let cpus = sys::worker_cpus()?;
    if cpus.is_empty() {
        return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
    }
    let n = cpus.len();

    // A worker owns its whole physical core, so it serves every sibling's queues.
    let workers: Vec<Vec<usize>> = cpus
        .iter()
        .map(|&c| sys::siblings(c).unwrap_or_else(|| vec![c]))
        .collect();

    let ctl = ublk::Control::open()?;

    // Probe for the features we use; never test a version number.
    ctl.require(ublk::F_AUTO_BUF_REG | ublk::F_USER_COPY | ublk::F_CMD_IOCTL_ENCODE)?;

    let fabric = Arc::new(super::hop::Fabric::new(n, limits())?);
    let doorbell = Doorbell::new(fabric.clone())?;

    // Start workers
    let (ready_tx, ready_rx) = channel::<()>();
    let mut inboxes = Vec::with_capacity(n);
    let mut handles = Vec::with_capacity(n);
    for (core, &cpu) in cpus.iter().enumerate() {
        let (tx, rx) = channel::<Ctl<H::Config>>();
        inboxes.push(tx);
        let args = super::worker::WorkerArgs {
            core,
            cpu,
            fabric: fabric.clone(),
            inbox: rx,
            ready: ready_tx.clone(),
        };
        handles.push(kernel::spawn(
            format!("racer-w{core}"),
            super::worker::worker_task::<H>(args),
        )?);
    }
    drop(ready_tx);
    while kernel::recv(&ready_rx).is_ok() {}

    // Start the control thread
    let (tx, rx) = channel::<Job<H::Config>>();
    let cpu0 = cpus[0];
    let core = Core::new(Some(ctl), workers);
    let control = kernel::spawn(
        "racer-ctl".into(),
        ControlTask::<H::Config> {
            parts: Some((core, inboxes, doorbell)),
            ctx: kernel::OnThread(None),
            rx,
            cpu0,
        },
    )?;

    Ok(Runtime {
        inner: Arc::new(Inner {
            tx: Mutex::new(Some(tx)),
            control: Mutex::new(Some(control)),
            workers: Mutex::new(handles),
            down: AtomicBool::new(false),
        }),
    })
}

/// The control thread: one job at a time, in the order they were posted.
///
/// Everything that changes a node's shape happens here and nowhere else, which is why it
/// is a queue and not a lock. It ends when the last sender goes, and the last sender goes
/// when the runtime is shut down.
struct ControlTask<C: Sync + 'static> {
    /// What the context is built from, until it is. The context itself holds versions of
    /// the handler's configuration, which is `Sync` but need not be `Send`, so it is
    /// assembled on the thread that will use it.
    parts: Option<ControlParts<C>>,
    ctx: kernel::OnThread<ThreadCtx<C>>,
    rx: Receiver<Job<C>>,
    cpu0: usize,
}

type ControlParts<C> = (Core, Vec<Sender<Ctl<C>>>, Doorbell);

impl<C: Sync + 'static> kernel::Task for ControlTask<C> {
    fn start(&mut self) {
        // A sibling of the first worker's CPU: close enough to share a cache, far enough
        // not to take a worker's turn away from it.
        if let Some(sib) = sys::sibling_of(self.cpu0) {
            let _ = sys::pin(sib);
        }
        let (core, inboxes, doorbell) = self.parts.take().expect("control started twice");
        self.ctx = kernel::OnThread(Some(ThreadCtx {
            resources: ResourceBuild {
                core: std::cell::RefCell::new(core),
            },
            hub: Hub { inboxes, doorbell },
            current: None,
            retiring: Vec::new(),
        }));
    }

    fn turn(&mut self) -> kernel::Turn {
        let Some(ctx) = self.ctx.0.as_mut() else {
            return kernel::Turn::Done;
        };
        control_turn(ctx, &self.rx)
    }
}

/// One job off the control queue.
///
/// The wait is a turn of its own, not a block inside one: the jobs this thread runs are
/// posted by threads it would otherwise be keeping from running.
fn control_turn<C: Sync + 'static>(ctx: &mut ThreadCtx<C>, rx: &Receiver<Job<C>>) -> kernel::Turn {
    const SWEEP_INTERVAL: Duration = Duration::from_millis(1);
    let wait = if ctx.retiring.is_empty() {
        match kernel::recv_turn(rx) {
            kernel::Wait::Got(job) => Ok(job),
            kernel::Wait::Idle => return kernel::Turn::Idle,
            kernel::Wait::Closed => return kernel::Turn::Done,
        }
    } else {
        kernel::recv_timeout(rx, SWEEP_INTERVAL)
    };
    match wait {
        Ok(job) => {
            job(ctx);
            kernel::Turn::Ran
        }
        Err(RecvTimeoutError::Timeout) => {
            sweep(ctx);
            kernel::Turn::Idle
        }
        Err(RecvTimeoutError::Disconnected) => kernel::Turn::Done,
    }
}

fn reconcile<C: Sync + 'static, F>(ctx: &mut ThreadCtx<C>, build: F) -> Result<bool, UpdateError>
where
    F: FnOnce(&ResourceBuild, Option<&C>) -> std::io::Result<Option<C>>,
{
    {
        let mut c = ctx.resources.core.borrow_mut();
        c.new_files.clear();
        c.new_devs.clear();
        c.declared_vols.clear();
    }

    // 1. Build. Disks open and devices are created, not started; failure is undone below.
    let current = ctx.current.as_deref();
    let cfg = match build(&ctx.resources, current) {
        Ok(Some(c)) => Arc::new(c),
        Ok(None) => {
            rollback(ctx);
            return Ok(false);
        }
        Err(e) => {
            rollback(ctx);
            return Err(UpdateError::Candidate(e));
        }
    };

    let (new_files, new_devs, retiring) = {
        let c = ctx.resources.core.borrow();
        let retiring: Vec<(u16, u32)> = c
            .vols
            .iter()
            .filter(|v| !c.declared_vols.contains(&v.key) && v.weak.strong_count() > 0)
            .map(|v| (v.slot, v.dev_id))
            .collect();
        (c.new_files.clone(), c.new_devs.clone(), retiring)
    };

    // 2. Register new files on every core before anything can name them.
    if !new_files.is_empty() {
        ctx.hub.broadcast("RegisterFiles", |_, ack| {
            Ctl::RegisterFiles(new_files.clone(), ack)
        });
    }

    // 3. Retiring devices stop accepting IO; in-flight requests hold the old config.
    //
    // The order within each device is load bearing. STOP_DEV deletes the gendisk, and
    // deleting it waits for the request queue to freeze; the queue cannot freeze while
    // a consumer outside this process still has reads outstanding. The kernel aborts
    // those only when the last char-device reference goes away. So the workers stop
    // dispatching first, then we drop our handle, and only then do we ask for the disk.
    // Asking first parks the configuration thread in `blk_mq_freeze_queue_wait` for as
    // long as the consumer keeps reading, and every later configuration is ignored in
    // silence because the reload never returns.
    //
    // Draining is two phases for a matching reason. A worker's tags sit parked in
    // `UBLK_IO_FETCH_REQ`, and the only thing that completes them is the STOP_DEV
    // below; a single drain that waited for them would wait for the step it is holding
    // up, which is the same silent stall from the other direction. So phase one waits
    // only for our own in-flight requests, and phase two, after STOP_DEV, collects the
    // tags the kernel hands back. The slot stays pinned across both: a tag id names a
    // device slot and carries no epoch, so releasing it while abort completions are
    // still in the ring would let a stale one land on whatever device took the slot.
    for &(slot, dev_id) in &retiring {
        stop_and_reap(ctx, slot, dev_id);
    }

    // 4. Install: each worker builds and swaps its complete generation value locally.
    ctx.hub.broadcast("Install", |_, ack| Ctl::Install {
        config: cfg.clone(),
        ack,
    });

    // 5. Arm and start new devices; the config describing them is live on every core.
    for slot in &new_devs {
        let (dev_id, dev, q_ids, cpath) = {
            let c = ctx.resources.core.borrow();
            let v = c.vols.iter().find(|v| v.slot == *slot).unwrap();
            (
                v.dev_id,
                v.key,
                v.q_ids.clone(),
                ublk::char_dev_path(v.dev_id),
            )
        };
        let cdev = kernel::open(Path::new(&cpath), libc::O_RDWR | libc::O_CLOEXEC, 0)
            .map_err(UpdateError::Runtime)?;
        let named = cdev.as_ref();
        let depth = ctx.resources.core.borrow().limits.queue_depth;
        ctx.hub.broadcast("StartQueue", |i, ack| {
            Ctl::Ublk(ublk::start_queue(
                *slot,
                dev,
                named,
                depth,
                q_ids[i].clone(),
                ack,
            ))
        });
        let mut c = ctx.resources.core.borrow_mut();
        c.ctl()
            .start_dev(dev_id, std::process::id() as i32)
            .map_err(UpdateError::Runtime)?;
        if let Some(v) = c.vols.iter_mut().find(|v| v.slot == *slot) {
            v.cdev = Some(cdev);
        }
    }

    if let Some(old) = ctx.current.replace(cfg) {
        ctx.retiring.push(old);
    }
    sweep(ctx);
    Ok(true)
}

/// Undo a failed build: close the fds it opened and delete the devices it created.
fn rollback<C>(ctx: &mut ThreadCtx<C>) {
    let mut c = ctx.resources.core.borrow_mut();
    let new_files = std::mem::take(&mut c.new_files);
    for (slot, _) in new_files {
        c.file_used[slot as usize] = false;
        c.disks.retain(|d| d.slot != slot);
    }
    let new_devs = std::mem::take(&mut c.new_devs);
    for slot in new_devs {
        if let Some(pos) = c.vols.iter().position(|v| v.slot == slot) {
            let dev_id = c.vols[pos].dev_id;
            // Safe to wait here, unlike in `reap` and `teardown`: this build never
            // reached `start_dev`, so the minor has no gendisk and therefore no consumer
            // outside this process. Waiting frees the minor now rather than leaving it
            // pinned until the kernel gets round to it.
            let _ = c.ctl().del_dev(dev_id);
            c.vols.remove(pos);
        }
        c.dev_used[slot as usize] = false;
    }
}

/// Drop control's retiring generations once all workers and requests have released them.
fn sweep<C>(ctx: &mut ThreadCtx<C>) {
    ctx.retiring.retain(|cfg| Arc::strong_count(cfg) > 1);
    reclaim(ctx);
}

/// Free resources once no current or retiring configuration still holds their handles.
fn reclaim<C>(ctx: &mut ThreadCtx<C>) {
    let dead_files: Vec<u32> = {
        let mut c = ctx.resources.core.borrow_mut();
        let dead: Vec<u32> = c
            .disks
            .iter()
            .filter(|d| d.weak.strong_count() == 0)
            .map(|d| d.slot)
            .collect();
        c.disks.retain(|d| d.weak.strong_count() > 0);
        for s in &dead {
            c.file_used[*s as usize] = false;
        }
        dead
    };
    if !dead_files.is_empty() {
        ctx.hub.broadcast("UnregisterFiles", |_, ack| {
            Ctl::UnregisterFiles(dead_files.clone(), ack)
        });
    }

    let mut c = ctx.resources.core.borrow_mut();
    let dead_vols: Vec<(u16, u32)> = c
        .vols
        .iter()
        .filter(|v| v.weak.strong_count() == 0)
        .map(|v| (v.slot, v.dev_id))
        .collect();
    for (slot, dev_id) in dead_vols {
        // Drop our own char-device handle; workers dropped theirs on drain. That is all
        // we control: the block device may still be open by a consumer outside this
        // process, and a synchronous DEL_DEV would park the configuration thread in the
        // kernel until that consumer let go. Nothing here is allowed to wait on someone
        // else's file descriptor, so ask asynchronously and let the kernel free the
        // minor whenever the last holder closes.
        if let Some(v) = c.vols.iter_mut().find(|v| v.slot == slot) {
            v.cdev = None;
        }
        let _ = c.ctl().del_dev_async(dev_id);
        c.dev_used[slot as usize] = false;
        c.vols.retain(|v| v.slot != slot);
    }
}

fn stop_and_reap<C>(ctx: &mut ThreadCtx<C>, slot: u16, dev_id: u32) {
    ctx.hub
        .broadcast("StopQueue", |_, ack| Ctl::Ublk(ublk::stop_queue(slot, ack)));

    {
        let mut c = ctx.resources.core.borrow_mut();
        if let Some(v) = c.vols.iter_mut().find(|v| v.slot == slot) {
            v.cdev = None;
        }
        let _ = c.ctl().stop_dev(dev_id);
    }

    ctx.hub
        .broadcast("ReapQueue", |_, ack| Ctl::Ublk(ublk::reap_queue(slot, ack)));
}

fn teardown<C>(ctx: &mut ThreadCtx<C>) -> std::io::Result<()> {
    let live: Vec<(u16, u32)> = ctx
        .resources
        .core
        .borrow()
        .vols
        .iter()
        .map(|v| (v.slot, v.dev_id))
        .collect();
    for &(slot, dev_id) in &live {
        // Same ordering as a reconfiguration, and two phases for the same reason: stop
        // dispatching, drop our char-device handle, delete the disk, and only then
        // collect the tags STOP_DEV handed back. A consumer with reads outstanding
        // would otherwise freeze the queue against us and hold shutdown open until it
        // gave up, and being killed for that leaves the export in a worse state.
        stop_and_reap(ctx, slot, dev_id);
    }

    ctx.hub.broadcast("Shutdown", |_, ack| Ctl::Shutdown(ack));

    if let Some(current) = ctx.current.take() {
        ctx.retiring.push(current);
    }
    let (_wait_tx, wait_rx) = channel::<()>();
    while ctx.retiring.iter().any(|cfg| Arc::strong_count(cfg) > 1) {
        let _ = kernel::recv_timeout(&wait_rx, Duration::from_millis(1));
        sweep(ctx);
    }
    ctx.retiring.clear();

    let mut c = ctx.resources.core.borrow_mut();
    // The char-device handles went with the stop above, so ask for the devices to go
    // away without waiting: a consumer that still has the block device open would
    // otherwise hold shutdown open indefinitely, and being killed for it leaves the
    // export in a worse state than letting the kernel reclaim the minor once that
    // consumer closes.
    for (_, dev_id) in live {
        let _ = c.ctl().del_dev_async(dev_id);
    }
    c.vols.clear();
    c.disks.clear();
    Ok(())
}

/// Why a generation update failed.
#[derive(Debug)]
pub enum UpdateError {
    /// The candidate was rejected before it became visible to workers.
    Candidate(std::io::Error),
    /// The runtime failed after committing the candidate; continuing could diverge from
    /// the control plane.
    Runtime(std::io::Error),
}

impl UpdateError {
    pub fn into_inner(self) -> std::io::Error {
        match self {
            UpdateError::Candidate(e) | UpdateError::Runtime(e) => e,
        }
    }
}

impl std::fmt::Display for UpdateError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            UpdateError::Candidate(e) | UpdateError::Runtime(e) => e.fmt(f),
        }
    }
}

impl std::error::Error for UpdateError {}

struct DiskEntry {
    path: PathBuf,
    slot: u32,
    weak: Weak<DiskInner>,
}

struct VolEntry {
    key: u64,
    size: u64,
    /// The ublk minor asked for, which the config froze along with the size.
    minor: u32,
    /// The geometry the export was created with, frozen for the same reason.
    params: ublk::Params,
    slot: u16,
    dev_id: u32,
    /// Hardware queues each worker serves, indexed by worker.
    q_ids: Vec<Vec<u16>>,
    weak: Weak<ExportInner>,
    /// Stays open for the device's life: ublk cancels fetches when the last ref drops.
    cdev: Option<kernel::File>,
}

struct Core {
    /// The sizes this node was built at, captured where the runtime was started.
    limits: &'static Limits,
    /// Absent in the simulator; used only on the control thread's reconfiguration path.
    ctl: Option<ublk::Control>,
    /// Logical CPUs owned by each worker, in worker order.
    workers: Vec<Vec<usize>>,
    disks: Vec<DiskEntry>,
    vols: Vec<VolEntry>,
    file_used: Vec<bool>,
    dev_used: Vec<bool>,
    /// Slots opened during the current `reload`, so a failed build can be undone.
    new_files: Vec<(u32, kernel::FileRef)>,
    new_devs: Vec<u16>,
    declared_vols: Vec<u64>,
}

impl Core {
    fn new(ctl: Option<ublk::Control>, workers: Vec<Vec<usize>>) -> Core {
        let limits = limits();
        Core {
            limits,
            ctl,
            workers,
            disks: Vec::new(),
            vols: Vec::new(),
            file_used: vec![false; limits.file_slots as usize],
            dev_used: vec![false; limits.max_devices as usize],
            new_files: Vec::new(),
            new_devs: Vec::new(),
            declared_vols: Vec::new(),
        }
    }

    fn ctl(&mut self) -> &mut ublk::Control {
        self.ctl.as_mut().expect("no ublk control plane")
    }
}

/// Resources available while building one runtime generation.
///
/// Handles returned here stay registered until every generation holding them retires.
pub struct ResourceBuild {
    core: std::cell::RefCell<Core>,
}

impl ResourceBuild {
    /// Number of workers; configs are built on the control thread, where `core()` panics.
    pub(crate) fn cores(&self) -> usize {
        self.core.borrow().workers.len()
    }

    /// A file or block device this node reads and writes directly.
    ///
    /// `timeout` bounds every IO; use it over the fabric, where expiry should surface as
    /// `ETIME` rather than a stuck request. `limit` paces submissions. Local storage and
    /// a peer's device pass `None`; a peer's device is metered by its owner.
    pub(crate) fn disk(
        &self,
        path: &Path,
        timeout: Option<Duration>,
        limit: Option<Limiter>,
    ) -> std::io::Result<Disk> {
        let mut c = self.core.borrow_mut();
        // Re-declaring a path reuses the registration; a reload never disturbs a live fd.
        if let Some(d) = c
            .disks
            .iter()
            .find(|d| d.path == path)
            .and_then(|d| d.weak.upgrade())
        {
            return Ok(Disk::from_inner(d));
        }
        let limit = limit.unwrap_or_else(|| Limiter::new(0, 0));
        let inner = {
            let file = sys::open_direct(path)?;
            let slot = (c.limits.disk_file_base()..c.limits.file_slots)
                .find(|s| !c.file_used[*s as usize])
                .ok_or_else(|| std::io::Error::from_raw_os_error(libc::ENOSPC))?;
            c.file_used[slot as usize] = true;
            c.new_files.push((slot, file.as_ref()));
            Arc::new(DiskInner {
                slot,
                timeout,
                limit,
                file,
            })
        };
        c.disks.push(DiskEntry {
            path: path.to_path_buf(),
            slot: inner.slot,
            weak: Arc::downgrade(&inner),
        });
        Ok(Disk::from_inner(inner))
    }

    /// A block device this node exports.
    ///
    /// `key` is stable identity: re-declaring it keeps the same device and never
    /// interrupts IO. `minor` is the ublk device number asked of the kernel, so the export
    /// appears at `/dev/ublkb<minor>` and peers and mounts find it where the control plane
    /// said they would. `size` is in bytes, a nonzero multiple of 4 KiB, and neither it
    /// nor the minor may change while the export lives.
    pub(crate) fn device(&self, key: u64, minor: u32, size: u64) -> std::io::Result<Export> {
        if !size.is_multiple_of(4096) || size == 0 {
            return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
        }
        self.declare(key, minor, size, ublk::params_for(size))
    }

    /// The node's fabric device: the one namespace peers issue against.
    /// Same machinery as [`ResourceBuild::device`], different geometry: a fabric frame is
    /// one or two blocks depending on the opcode, so it is not cut at a block boundary.
    pub(crate) fn fabric(&self, key: u64, minor: u32, size: u64) -> std::io::Result<Export> {
        if !size.is_multiple_of(4096) || size == 0 {
            return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
        }
        self.declare(key, minor, size, ublk::params_for_fabric(size))
    }

    fn declare(
        &self,
        key: u64,
        minor: u32,
        size: u64,
        params: ublk::Params,
    ) -> std::io::Result<Export> {
        let mut c = self.core.borrow_mut();
        c.declared_vols.push(key);
        if let Some(v) = c.vols.iter().find(|v| v.key == key) {
            // Exports are immutable once created: a resize, a move to another minor or a
            // change of geometry is a config error, not a live device to reshape.
            if v.size != size || v.minor != minor || v.params != params {
                return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
            }
            if let Some(inner) = v.weak.upgrade() {
                return Ok(Export::from_inner(inner));
            }
        }
        let slot = (0..c.limits.max_devices)
            .find(|s| !c.dev_used[*s as usize])
            .ok_or_else(|| std::io::Error::from_raw_os_error(libc::ENOSPC))?;

        let (dev_id, q_ids, path) = {
            // One hardware queue per logical CPU we own; blk-mq maps each to a core we
            // run on.
            let nq = c.workers.len() * QUEUES_PER_WORKER;
            let mut info = ublk::DevInfo::new(
                minor,
                nq as u16,
                c.limits.queue_depth,
                ublk::F_AUTO_BUF_REG | ublk::F_USER_COPY | ublk::F_CMD_IOCTL_ENCODE,
            );
            let held = c.dev_used.iter().filter(|u| **u).count();
            let Core { ctl, workers, .. } = &mut *c;
            let ctl = ctl.as_mut().expect("no ublk control plane");
            ublk::add_dev(ctl, &mut info, held)?;
            let dev_id = info.dev_id;
            let setup = ctl
                .set_params(dev_id, &params)
                .and_then(|()| ublk::assign_queues(ctl, dev_id, nq, workers, QUEUES_PER_WORKER));
            match setup {
                Ok(q) => (dev_id, q, PathBuf::from(ublk::block_dev_path(dev_id))),
                Err(e) => {
                    let _ = ctl.del_dev(dev_id);
                    return Err(e);
                }
            }
        };
        c.dev_used[slot as usize] = true;
        c.new_devs.push(slot);

        let inner = Arc::new(ExportInner { path });
        c.vols.retain(|v| v.key != key);
        c.vols.push(VolEntry {
            key,
            size,
            minor,
            params,
            slot,
            dev_id,
            q_ids,
            weak: Arc::downgrade(&inner),
            cdev: None,
        });
        Ok(Export::from_inner(inner))
    }
}

/// The control thread's channel to every worker: a queue plus a doorbell to wake it.
struct Hub<C> {
    inboxes: Vec<Sender<Ctl<C>>>,
    doorbell: Doorbell,
}

const BROADCAST_WARN: Duration = Duration::from_secs(5);
/// Number of control broadcasts that have stalled since boot.
pub(crate) fn broadcast_stalls() -> u64 {
    kernel::counter(kernel::Counter::BroadcastStalls)
}

/// How long the control thread has been stuck in its current broadcast, in microseconds.
pub(crate) fn broadcast_wait_us() -> u64 {
    kernel::counter(kernel::Counter::BroadcastWaitUs)
}

impl<C> Hub<C> {
    #[cfg(test)]
    fn request(
        &mut self,
        core: usize,
        req: super::Request,
        done: Sender<Result<(), super::Errno>>,
    ) -> std::io::Result<()> {
        let inbox = self
            .inboxes
            .get(core)
            .ok_or_else(|| std::io::Error::from_raw_os_error(libc::EINVAL))?;
        inbox
            .send(Ctl::Request { id: 0, req, done })
            .map_err(|_| std::io::Error::from_raw_os_error(libc::EPIPE))?;
        self.doorbell.wake(core);
        Ok(())
    }

    /// Broadcast one message and block until every worker drops its `Ack`, done or dead.
    fn broadcast(&mut self, what: &str, mut make: impl FnMut(usize, Ack) -> Ctl<C>) {
        let (tx, rx) = channel::<()>();
        let mut sent = 0usize;
        for i in 0..self.inboxes.len() {
            if self.inboxes[i].send(make(i, tx.clone())).is_ok() {
                self.doorbell.wake(i);
                sent += 1;
            }
        }
        drop(tx);
        let start = super::now();
        let mut acked = 0usize;
        let mut warned = false;
        loop {
            match kernel::recv_timeout(&rx, BROADCAST_WARN) {
                Ok(()) => acked += 1,
                Err(RecvTimeoutError::Disconnected) => break,
                Err(RecvTimeoutError::Timeout) => {
                    let waited = super::now().saturating_duration_since(start);
                    kernel::set_counter(
                        kernel::Counter::BroadcastWaitUs,
                        waited.as_micros() as u64,
                    );
                    if !warned {
                        kernel::add_counter(kernel::Counter::BroadcastStalls, 1);
                        warned = true;
                    }
                    eprintln!(
                        "racer: control broadcast {what} stalled for {}s: {} of {sent} workers \
                         have acked; no later configuration can be applied until they do",
                        waited.as_secs(),
                        acked,
                    );
                }
            }
        }
        if warned {
            kernel::set_counter(kernel::Counter::BroadcastWaitUs, 0);
            eprintln!(
                "racer: control broadcast {what} completed after {}s",
                super::now().saturating_duration_since(start).as_secs(),
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::sync::mpsc::{Receiver, channel};

    use crate::runtime::Fabric;

    use super::{
        Core, Ctl, Doorbell, Hub, ResourceBuild, ThreadCtx, UpdateError, reconcile, sweep,
    };

    #[derive(Debug, PartialEq, Eq)]
    enum Event {
        Install(u8),
    }

    fn harness() -> (ThreadCtx<u8>, Receiver<Event>, std::thread::JoinHandle<()>) {
        let fabric = Arc::new(Fabric::new(1, super::limits()).unwrap());
        let doorbell = Doorbell::new(fabric).unwrap();
        let (ctl_tx, ctl_rx) = channel();
        let (event_tx, event_rx) = channel();
        let worker = std::thread::spawn(move || {
            while let Ok(msg) = ctl_rx.recv() {
                let (event, ack) = match msg {
                    Ctl::Install { config, ack } => (Event::Install(*config), ack),
                    _ => panic!("unexpected control message"),
                };
                event_tx.send(event).unwrap();
                ack.send(()).unwrap();
            }
        });
        let ctx = ThreadCtx {
            resources: ResourceBuild {
                core: std::cell::RefCell::new(Core::new(None, vec![Vec::new()])),
            },
            hub: Hub {
                inboxes: vec![ctl_tx],
                doorbell,
            },
            current: None,
            retiring: Vec::new(),
        };
        (ctx, event_rx, worker)
    }

    #[test]
    fn reconcile_candidate_lifecycle() {
        let (mut ctx, events, worker) = harness();

        assert!(
            reconcile(&mut ctx, |_, current| {
                assert!(current.is_none());
                Ok(Some(1u8))
            })
            .unwrap()
        );
        assert_eq!(events.recv().unwrap(), Event::Install(1));

        assert!(!reconcile(&mut ctx, |_, _| Ok(None)).unwrap());
        let err = reconcile(&mut ctx, |_, _| {
            Err::<Option<u8>, _>(std::io::Error::from_raw_os_error(libc::EINVAL))
        })
        .unwrap_err();
        assert!(matches!(
            err,
            UpdateError::Candidate(e) if e.raw_os_error() == Some(libc::EINVAL)
        ));
        assert!(matches!(
            events.try_recv(),
            Err(std::sync::mpsc::TryRecvError::Empty)
        ));

        assert!(
            reconcile(&mut ctx, |_, current| {
                assert_eq!(current, Some(&1));
                Ok(Some(2))
            })
            .unwrap()
        );
        assert_eq!(events.recv().unwrap(), Event::Install(2));
        assert_eq!(ctx.current.as_deref(), Some(&2));
        assert!(ctx.retiring.is_empty());

        drop(ctx);
        worker.join().unwrap();
    }

    #[test]
    fn retiring_generation_waits_for_its_last_external_owner() {
        let (mut ctx, events, worker) = harness();

        assert!(reconcile(&mut ctx, |_, _| Ok(Some(1u8))).unwrap());
        assert_eq!(events.recv().unwrap(), Event::Install(1));
        let request = ctx.current.as_ref().unwrap().clone();

        assert!(reconcile(&mut ctx, |_, _| Ok(Some(2u8))).unwrap());
        assert_eq!(events.recv().unwrap(), Event::Install(2));
        assert_eq!(ctx.retiring.len(), 1);

        sweep(&mut ctx);
        assert_eq!(ctx.retiring.len(), 1);
        drop(request);
        sweep(&mut ctx);
        assert!(ctx.retiring.is_empty());

        drop(ctx);
        worker.join().unwrap();
    }
}
