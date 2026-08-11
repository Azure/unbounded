//! Racer runtime: ublk block devices served by one io_uring worker per physical core.
//!
//! Resources are declared, never opened ad hoc: [`Runtime::reload`] hands you a
//! [`Configurator`] and you build your config out of the handles it returns, which stay
//! registered on every worker until that config is retired. Holding a [`Disk`] is the
//! proof that it is registered, so IO cannot name a file the kernel does not know about.
//!
//! Under the `sim` feature the kernel seams are replaced by `sim`: no ublk device is
//! ever created, a [`Disk`] names a simulated one, and the tables below shrink.

mod exec;
mod hop;
mod io;
mod limit;
mod sys;
mod ublk;
mod worker;

use std::collections::VecDeque;
use std::marker::PhantomData;
use std::ops::Deref;
use std::os::fd::{AsRawFd, RawFd};
use std::path::{Path, PathBuf};
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{Sender, channel};
use std::sync::{Arc, Mutex, Weak};
use std::task::{Context, Poll};
use std::time::{Duration, Instant};

pub(crate) use io::{Buf, Disk, Durability, PoolBuf, Volume, sleep};
pub(crate) use limit::Limiter;
pub(crate) use worker::core;

#[cfg(feature = "sim")]
pub(crate) use io::{sim_addr, sim_buf, sim_complete};

#[cfg(feature = "sim")]
/// Request slots one worker preallocates; the simulator hands them out itself.
pub(crate) fn sim_slots() -> u32 {
    MAX_DEVICES as u32 * TAGS_PER_DEV
}
#[cfg(feature = "sim")]
pub(crate) use worker::sim::{SimNode, SimWorker};

use io::{DiskInner, VolumeInner};
use worker::{Ack, Ctl, Doorbell};

/// Tags per ublk queue: in-flight requests per device per queue.
#[cfg(not(feature = "sim"))]
const QUEUE_DEPTH: u16 = 64;
/// A worker owns a physical core, so it serves both SMT siblings' hardware queues.
const QUEUES_PER_WORKER: usize = 2;
/// Registered buffer indices are dense over `(dev_slot, local_queue, tag)`.
const TAGS_PER_DEV: u32 = QUEUES_PER_WORKER as u32 * QUEUE_DEPTH as u32;
/// 60 devices keeps the registered buffer table inside the kernel's limit.
#[cfg(not(feature = "sim"))]
const MAX_DEVICES: u16 = 60;

// Simulated dimensions: hundreds of nodes share one address space, so per-worker
// tables shrink to the smallest size the protocol still fits in. These are counts,
// never rules: nothing changes but that slabs run out sooner, the interesting case.
#[cfg(feature = "sim")]
const QUEUE_DEPTH: u16 = 4;
#[cfg(feature = "sim")]
const MAX_DEVICES: u16 = 8;

/// Largest single request the block layer may send: one 4 MiB page, so an immutable
/// huge page is always filled by exactly one request.
const MAX_IO_BYTES: usize = 4 << 20;
/// Registered file table: `0..MAX_DEVICES` are ublk char devices, the rest are disks.
#[cfg(not(feature = "sim"))]
const FILE_SLOTS: u32 = 512;
#[cfg(feature = "sim")]
const FILE_SLOTS: u32 = 32;
const DISK_FILE_BASE: u32 = MAX_DEVICES as u32;
const REQ_BUF_SLOTS: u32 = MAX_DEVICES as u32 * TAGS_PER_DEV;
const POOL_BUF_BASE: u32 = REQ_BUF_SLOTS;
const TOTAL_BUF_SLOTS: u32 = REQ_BUF_SLOTS + io::POOL_BUFS;

#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub(crate) enum Op {
    Read,
    Write,
    Discard,
}

/// One block-layer request. `vol` is the volume's declared key, `lba` is in 4 KiB
/// units, and `buf` is the guest's own pages, already registered with io_uring, so IO
/// against it never copies.
#[derive(Copy, Clone, Debug)]
pub struct Request {
    pub(crate) vol: u64,
    pub(crate) op: Op,
    pub(crate) lba: u64,
    pub(crate) buf: Buf,
}

impl Request {
    /// Copy `dst.len()` bytes of a write request's payload out of the guest, starting
    /// at `off`.
    ///
    /// The escape hatch from [`Buf`]'s opacity: a 4 KiB payload must be staged in our
    /// own memory to be checksummed or parsed. ublk's `USER_COPY` does the copy in the
    /// kernel with a bounded `memcpy`, reached by a plain `pread(2)` on the char device
    /// because io_uring rejects a fixed buffer there and would punt a plain one to
    /// io-wq. 4 MiB pages never take this path.
    pub(crate) fn load(&self, off: usize, dst: &mut [u8]) -> Result<(), Errno> {
        #[cfg(feature = "sim")]
        return crate::sim::copy_req(self.buf, off, dst.as_mut_ptr(), dst.len(), false);
        #[cfg(not(feature = "sim"))]
        worker::copy_req(self.buf.index as u32, off, dst.as_mut_ptr(), dst.len(), false)
    }

    /// Copy `src` into a read request's payload at `off`. The mirror of [`load`].
    ///
    /// [`load`]: Request::load
    pub(crate) fn store(&self, off: usize, src: &[u8]) -> Result<(), Errno> {
        #[cfg(feature = "sim")]
        return crate::sim::copy_req(self.buf, off, src.as_ptr() as *mut u8, src.len(), true);
        #[cfg(not(feature = "sim"))]
        worker::copy_req(self.buf.index as u32, off, src.as_ptr() as *mut u8, src.len(), true)
    }
}

/// A raw errno, as returned to the block layer.
#[derive(Copy, Clone, PartialEq, Eq)]
pub struct Errno(i32);

impl Errno {
    pub(crate) const EIO: Errno = Errno(libc::EIO);
    pub(crate) const ENOSPC: Errno = Errno(libc::ENOSPC);
    pub(crate) const EOPNOTSUPP: Errno = Errno(libc::EOPNOTSUPP);
    pub(crate) const EINVAL: Errno = Errno(libc::EINVAL);
    // `fabric` names its statuses out of these. The usable set is a property of this
    // boundary: an errno survives to the initiator only if nvmet's
    // `blk_to_nvme_status()` has an arm for it, and it has four. A code the target
    // erases would read as a transport failure and trigger a path failover.
    pub(crate) const ENODATA: Errno = Errno(libc::ENODATA);
    pub(crate) const EREMOTEIO: Errno = Errno(libc::EREMOTEIO);

    pub(crate) fn from_raw(e: i32) -> Errno {
        Errno(e.abs())
    }
    pub(crate) fn raw(self) -> i32 {
        self.0
    }
}

impl std::fmt::Debug for Errno {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Errno({})", self.0)
    }
}

/// A borrow of the live configuration, pinned for as long as it is held.
///
/// `Cfg` is `!Send`: its reference count is a plain per-core counter, which keeps
/// reload off the hot path. A version is retired only once every core reports zero
/// outstanding guards.
pub struct Cfg<C> {
    ptr: *const C,
    ver: u32,
    _nosend: PhantomData<*const ()>,
}

impl<C> Cfg<C> {
    fn new(ptr: *const C, ver: u32) -> Self {
        Cfg {
            ptr,
            ver,
            _nosend: PhantomData,
        }
    }
}

impl<C> Clone for Cfg<C> {
    fn clone(&self) -> Self {
        worker::with_local(|l| l.guard_inc(self.ver));
        Cfg {
            ptr: self.ptr,
            ver: self.ver,
            _nosend: PhantomData,
        }
    }
}

impl<C> Deref for Cfg<C> {
    type Target = C;
    fn deref(&self) -> &C {
        unsafe { &*self.ptr }
    }
}

impl<C> Drop for Cfg<C> {
    fn drop(&mut self) {
        worker::with_local(|l| l.guard_dec(self.ver));
    }
}

/// The dataplane. One instance, shared by every core, for the life of the process.
pub trait Handler: Sync + 'static {
    /// Everything the handler needs that the runtime must keep alive: disks, volumes,
    /// peer tables. Built by you inside [`Runtime::reload`].
    type Config: Sync + 'static;

    /// Serve one request. The returned future is stored in a preallocated slot, so it
    /// must be small; put cold paths behind `Box::pin(..).await`.
    fn handle(
        &'static self,
        cfg: Cfg<Self::Config>,
        req: Request,
    ) -> impl Future<Output = Result<(), Errno>> + 'static;

    /// Cooperative maintenance slot: runs on every core about every millisecond, idle
    /// or not, for per-core state nothing else may touch (allocator refills, cache
    /// decay, metric sampling). It runs on the worker thread, so it must not block.
    fn tick(&'static self, _cfg: Cfg<Self::Config>, _now: Instant) {}
}

/// Run `f` on `core` and await its result there.
///
/// If `core` is this core the closure runs inline, with no message. Otherwise it is
/// copied into the destination's SPSC ring and polled there; only the result travels
/// back. Dropping the returned future abandons the call: the destination still runs to
/// completion, but its result is discarded instead of delivered.
///
/// Sharding is the caller's business: pick `core` however your keyspace says to.
pub(crate) fn on_core<F, Fut, T>(core: usize, f: F) -> impl Future<Output = T>
where
    F: FnOnce() -> Fut + Send + 'static,
    Fut: Future<Output = T> + 'static,
    T: Send + 'static,
{
    hop::Hop::<F, Fut, T>::new(core, f)
}

/// Run `fut` on this core to completion, detached: no handle and no result.
///
/// [`Handler::tick`] is synchronous, so a maintenance step that has to wait on I/O —
/// the anti-entropy sweep in `heal` is the one that exists — needs somewhere else to
/// live. The task is parked in the same per-core slab as remote futures, so the
/// runtime already counts it when deciding it has quiesced.
///
/// Returns false if the slab is full, having dropped `fut` unpolled. Callers are
/// periodic by construction and skipping a round costs them nothing.
pub(crate) fn spawn(fut: impl Future<Output = ()> + 'static) -> bool {
    hop::spawn(fut)
}

// ---------------------------------------------------------------------------
// Fan-out / fan-in
//
// The combinator holds its futures inline, so a fan-out costs no allocation. A
// finished or abandoned future is dropped in place, never moved, which is what lets
// it hold `!Unpin` futures such as hops.
// ---------------------------------------------------------------------------

/// Poll all `futs` concurrently until `need` of them succeed, then abandon the rest.
///
/// Resolves as soon as the outcome is settled either way: `need` successes, or so many
/// failures that `need` is unreachable. Slot `i` holds `futs[i]`'s outcome, or `None`
/// if it was still running when the quorum resolved.
pub(crate) fn quorum<T, E, F, const N: usize>(
    futs: [F; N],
    need: usize,
) -> impl Future<Output = [Option<Result<T, E>>; N]>
where
    F: Future<Output = Result<T, E>>,
{
    Quorum {
        futs: futs.map(Some),
        out: [const { None }; N],
        need,
        ok: 0,
        failed: 0,
    }
}

struct Quorum<T, E, F, const N: usize> {
    futs: [Option<F>; N],
    out: [Option<Result<T, E>>; N],
    need: usize,
    ok: usize,
    failed: usize,
}

impl<T, E, F: Future<Output = Result<T, E>>, const N: usize> Future for Quorum<T, E, F, N> {
    type Output = [Option<Result<T, E>>; N];

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = unsafe { self.get_unchecked_mut() };
        for i in 0..N {
            let Some(f) = this.futs[i].as_mut() else {
                continue;
            };
            if let Poll::Ready(v) = unsafe { Pin::new_unchecked(f) }.poll(cx) {
                if v.is_ok() {
                    this.ok += 1;
                } else {
                    this.failed += 1;
                }
                this.out[i] = Some(v);
                this.futs[i] = None;
            }
        }
        if this.ok < this.need && N - this.failed >= this.need {
            return Poll::Pending;
        }
        // Settled: drop the losers in place, which abandons any hop still in flight.
        for f in this.futs.iter_mut() {
            *f = None;
        }
        Poll::Ready(std::array::from_fn(|i| this.out[i].take()))
    }
}

// ---------------------------------------------------------------------------
// Configurator
// ---------------------------------------------------------------------------

struct DiskEntry {
    path: PathBuf,
    slot: u32,
    weak: Weak<DiskInner>,
}

struct VolEntry {
    key: u64,
    size: u64,
    slot: u16,
    dev_id: u32,
    /// Hardware queues each worker serves, indexed by worker.
    q_ids: Vec<Vec<u16>>,
    weak: Weak<VolumeInner>,
    /// The char device stays open for the life of the device: ublk cancels every
    /// outstanding fetch as soon as the last reference to it goes away.
    cdev: Option<std::os::fd::OwnedFd>,
}

struct Core {
    /// Absent in the simulator, which calls the handler directly and so never creates
    /// a ublk device. Every use is on the control thread's reconfiguration path.
    ctl: Option<ublk::Control>,
    /// Logical CPUs owned by each worker, in worker order.
    workers: Vec<Vec<usize>>,
    disks: Vec<DiskEntry>,
    vols: Vec<VolEntry>,
    file_used: Vec<bool>,
    dev_used: Vec<bool>,
    /// Slots opened during the current `reload`, so a failed build can be undone.
    new_files: Vec<(u32, RawFd)>,
    new_devs: Vec<u16>,
    declared_vols: Vec<u64>,
}

impl Core {
    fn ctl(&mut self) -> &mut ublk::Control {
        self.ctl.as_mut().expect("no ublk control plane")
    }
}

/// Declares the resources a configuration needs. Every handle it returns stays
/// registered on every core until the configuration holding it is retired.
pub struct Configurator {
    core: std::cell::RefCell<Core>,
}

#[cfg(feature = "sim")]
impl Configurator {
    /// A configurator with no kernel behind it: disks resolve against the simulator's
    /// device table and a volume is only a name.
    pub(crate) fn sim(cores: usize) -> Configurator {
        Configurator {
            core: std::cell::RefCell::new(Core {
                ctl: None,
                workers: vec![Vec::new(); cores],
                disks: Vec::new(),
                vols: Vec::new(),
                file_used: vec![false; FILE_SLOTS as usize],
                dev_used: vec![false; MAX_DEVICES as usize],
                new_files: Vec::new(),
                new_devs: Vec::new(),
                declared_vols: Vec::new(),
            }),
        }
    }
}

impl Configurator {
    /// Number of workers this process runs. Available here because a configuration is
    /// built on the control thread, where `core()` would panic, and per-core state has
    /// to be sized before any worker sees it.
    pub(crate) fn cores(&self) -> usize {
        self.core.borrow().workers.len()
    }

    /// A file or block device this node reads and writes directly.
    ///
    /// `timeout` bounds every IO against it; use it for anything reached over the
    /// fabric, where expiry should surface as `ETIME` rather than a stuck request.
    /// Local storage is trusted and passes `None`.
    ///
    /// `limit` paces submissions to a rate the device is content to sustain. A peer's
    /// device passes `None`: it is metered by the node that owns it.
    pub(crate) fn disk(
        &self,
        path: &Path,
        timeout: Option<Duration>,
        limit: Option<Limiter>,
    ) -> std::io::Result<Disk> {
        let mut c = self.core.borrow_mut();
        // Re-declaring a path returns the same registration, so a live peer's fd is
        // never disturbed by a reload.
        if let Some(d) = c
            .disks
            .iter()
            .find(|d| d.path == path)
            .and_then(|d| d.weak.upgrade())
        {
            return Ok(Disk::from_inner(d));
        }
        let limit = limit.unwrap_or_else(|| Limiter::new(0, 0));
        // The simulator's device table names the slot directly: nothing to open and
        // nothing to register on a ring.
        #[cfg(feature = "sim")]
        let inner = Arc::new(DiskInner {
            slot: crate::sim::device(path)?,
            timeout,
            limit,
        });
        #[cfg(not(feature = "sim"))]
        let inner = {
            let fd = sys::open_direct(path)?;
            let slot = (DISK_FILE_BASE..FILE_SLOTS)
                .find(|s| !c.file_used[*s as usize])
                .ok_or_else(|| std::io::Error::from_raw_os_error(libc::ENOSPC))?;
            c.file_used[slot as usize] = true;
            c.new_files.push((slot, fd.as_raw_fd()));
            Arc::new(DiskInner {
                slot,
                timeout,
                limit,
                fd,
            })
        };
        c.disks.push(DiskEntry {
            path: path.to_path_buf(),
            slot: inner.slot,
            weak: Arc::downgrade(&inner),
        });
        Ok(Disk::from_inner(inner))
    }

    /// A block device this node exports. `key` is its stable identity: re-declaring
    /// the same key across reloads keeps the same `/dev/ublkbN` and never interrupts
    /// IO. `size` is in bytes, must be a nonzero multiple of the request unit below,
    /// and may never change.
    ///
    /// `huge` picks that unit: a huge volume is served in 4 MiB requests and a small
    /// one in 4 KiB, so the handler always sees exactly one page per request and never
    /// has to split or gather.
    pub(crate) fn volume(&self, key: u64, size: u64, huge: bool) -> std::io::Result<Volume> {
        let unit = if huge { 4 << 20 } else { 4096 };
        if !size.is_multiple_of(unit) || size == 0 {
            return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
        }
        self.declare(key, size, ublk::params_for(size, huge))
    }

    /// The node's fabric device: the one namespace peers issue against.
    ///
    /// Same machinery as [`volume`], different geometry. A fabric frame is 1, 2 or up
    /// to 1024 blocks depending on the opcode, so this device must not be split onto
    /// page boundaries the way a volume is.
    ///
    /// [`volume`]: Configurator::volume
    pub(crate) fn fabric(&self, key: u64, size: u64) -> std::io::Result<Volume> {
        if !size.is_multiple_of(4096) || size == 0 {
            return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
        }
        self.declare(key, size, ublk::params_for_fabric(size))
    }

    fn declare(&self, key: u64, size: u64, params: ublk::Params) -> std::io::Result<Volume> {
        let mut c = self.core.borrow_mut();
        c.declared_vols.push(key);
        if let Some(v) = c.vols.iter().find(|v| v.key == key) {
            // Volumes are immutable once created; a resize is a config error.
            if v.size != size {
                return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
            }
            if let Some(inner) = v.weak.upgrade() {
                return Ok(Volume::from_inner(inner));
            }
        }
        let slot = (0..MAX_DEVICES)
            .find(|s| !c.dev_used[*s as usize])
            .ok_or_else(|| std::io::Error::from_raw_os_error(libc::ENOSPC))?;

        // A simulated volume is a name and nothing else: requests reach the handler
        // with no kernel in between, so there is no device to create.
        #[cfg(feature = "sim")]
        let (dev_id, q_ids, path) = {
            let _ = params;
            (0u32, Vec::new(), PathBuf::from(format!("/sim/vol/{key}")))
        };
        #[cfg(not(feature = "sim"))]
        let (dev_id, q_ids, path) = {
            // One hardware queue per logical CPU we own, so blk-mq's own affinity map
            // lands each queue on a core we are already running on.
            let nq = c.workers.len() * QUEUES_PER_WORKER;
            let mut info = ublk::DevInfo::new(
                nq as u16,
                QUEUE_DEPTH,
                ublk::F_AUTO_BUF_REG | ublk::F_USER_COPY | ublk::F_CMD_IOCTL_ENCODE,
            );
            c.ctl().add_dev(&mut info)?;
            let dev_id = info.dev_id;
            let setup = c
                .ctl()
                .set_params(dev_id, &params)
                .and_then(|()| assign_queues(&mut c, dev_id, nq));
            match setup {
                Ok(q) => (dev_id, q, PathBuf::from(ublk::block_dev_path(dev_id))),
                Err(e) => {
                    let _ = c.ctl().del_dev(dev_id);
                    return Err(e);
                }
            }
        };
        c.dev_used[slot as usize] = true;
        c.new_devs.push(slot);

        let inner = Arc::new(VolumeInner { path });
        c.vols.retain(|v| v.key != key);
        c.vols.push(VolEntry {
            key,
            size,
            slot,
            dev_id,
            q_ids,
            weak: Arc::downgrade(&inner),
            cdev: None,
        });
        Ok(Volume::from_inner(inner))
    }
}

/// Map each hardware queue onto a worker that shares a CPU with it, spilling to the
/// least loaded worker when blk-mq picked a CPU we do not own or the home worker is
/// already full. Fails with `EINVAL` if every worker is full.
fn assign_queues(c: &mut Core, dev_id: u32, nq: usize) -> std::io::Result<Vec<Vec<u16>>> {
    let n = c.workers.len();
    let mut out: Vec<Vec<u16>> = vec![Vec::new(); n];
    let mut spill = Vec::new();
    for q in 0..nq as u16 {
        let mask = c.ctl().queue_affinity(dev_id, q)?;
        let home = c
            .workers
            .iter()
            .position(|cpus| mask.iter().any(|cpu| cpus.contains(cpu)));
        match home {
            Some(w) if out[w].len() < QUEUES_PER_WORKER => out[w].push(q),
            _ => spill.push(q),
        }
    }
    for q in spill {
        let w = (0..n).min_by_key(|w| out[*w].len()).unwrap();
        if out[w].len() >= QUEUES_PER_WORKER {
            return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
        }
        out[w].push(q);
    }
    Ok(out)
}

// ---------------------------------------------------------------------------
// Runtime
// ---------------------------------------------------------------------------

struct Version<C> {
    ver: u32,
    /// Owns the config while any core still holds a guard on it. Never read directly:
    /// workers see it through the raw pointer published at cutover.
    #[allow(dead_code)]
    cfg: Box<C>,
}

/// The control thread's channel to every worker: a message queue plus the doorbell
/// that wakes a parked worker to look at it.
struct Hub {
    inboxes: Vec<Sender<Ctl>>,
    doorbell: Doorbell,
}

impl Hub {
    /// Send one message to every worker and block until all of them are done with it.
    /// A worker releases us by dropping the `Ack` it was handed, so a worker that dies
    /// mid-message releases us instead of deadlocking.
    fn broadcast(&mut self, mut make: impl FnMut(usize, Ack) -> Ctl) {
        let (tx, rx) = channel::<()>();
        for i in 0..self.inboxes.len() {
            if self.inboxes[i].send(make(i, tx.clone())).is_ok() {
                self.doorbell.wake(i);
            }
        }
        drop(tx);
        // Workers send `()` before dropping their `Ack`; what ends the loop is the
        // last drop, not the sends.
        while rx.recv().is_ok() {}
    }
}

struct Ctx<C> {
    cfgr: Configurator,
    hub: Hub,
    versions: VecDeque<Version<C>>,
    next_ver: u32,
}

type Job<C> = Box<dyn FnOnce(&mut Ctx<C>) + Send>;

struct Inner<C> {
    tx: Mutex<Option<Sender<Job<C>>>>,
    control: Mutex<Option<std::thread::JoinHandle<()>>>,
    workers: Mutex<Vec<std::thread::JoinHandle<()>>>,
    down: AtomicBool,
}

/// Handle to a running runtime. Cloneable and shareable: a config watcher thread can
/// hold one and call [`Runtime::reload`] whenever the mesh changes. Dropping the last
/// clone shuts the runtime down.
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
    ///
    /// `build` runs on the runtime's control thread. Declare every disk, peer and
    /// volume you need; anything you do not re-declare is torn down once the previous
    /// configuration is retired, which happens only after the last request that could
    /// still see it has completed.
    pub fn reload<F>(&self, build: F) -> std::io::Result<()>
    where
        F: FnOnce(&Configurator) -> std::io::Result<C> + Send + 'static,
    {
        if self.inner.down.load(Ordering::Acquire) {
            return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
        }
        let (tx, rx) = channel();
        post(&self.inner, Box::new(move |ctx| {
            let _ = tx.send(reconcile(ctx, build));
        }))?;
        rx.recv()
            .map_err(|_| std::io::Error::from_raw_os_error(libc::EPIPE))?
    }

    /// Stop serving, tear every device down, and wait for the workers to exit.
    /// Idempotent; called automatically when the last handle drops.
    pub fn shutdown(&self) -> std::io::Result<()> {
        stop(&self.inner)
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

/// Shared by `shutdown` and `Drop`, which cannot carry the `C: Sync` bound.
fn stop<C>(inner: &Inner<C>) -> std::io::Result<()> {
    if inner.down.swap(true, Ordering::AcqRel) {
        return Ok(());
    }
    let (tx, rx) = channel();
    let posted = post(inner, Box::new(move |ctx| {
        let _ = tx.send(teardown(ctx));
    }));
    let r = match posted {
        Ok(()) => rx
            .recv()
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
    RUNNING.store(false, Ordering::Release);
    r
}

/// One runtime at a time: it owns every core in the affinity mask.
static RUNNING: AtomicBool = AtomicBool::new(false);

/// `RUNNING` makes a second [`start`] fail, so tests that boot a runtime must queue up
/// rather than run in parallel. Hold this for the life of the runtime. A panicking test
/// poisons the lock; the next one takes it anyway.
#[cfg(test)]
pub(crate) fn exclusive() -> std::sync::MutexGuard<'static, ()> {
    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    LOCK.lock().unwrap_or_else(|p| p.into_inner())
}

/// Boot the runtime: one pinned worker per physical core in the process affinity mask.
///
/// No devices exist yet; call [`Runtime::reload`] to declare them.
pub fn start<H: Handler>(handler: &'static H) -> std::io::Result<Runtime<H::Config>> {
    if RUNNING.swap(true, Ordering::AcqRel) {
        return Err(std::io::Error::from_raw_os_error(libc::EEXIST));
    }
    match boot(handler) {
        Ok(rt) => Ok(rt),
        Err(e) => {
            RUNNING.store(false, Ordering::Release);
            Err(e)
        }
    }
}

fn boot<H: Handler>(handler: &'static H) -> std::io::Result<Runtime<H::Config>> {
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

    let fabric = Arc::new(hop::Fabric::new(n)?);
    let doorbell = Doorbell::new(fabric.clone())?;

    let (ready_tx, ready_rx) = channel::<()>();
    let mut inboxes = Vec::with_capacity(n);
    let mut handles = Vec::with_capacity(n);
    for (core, &cpu) in cpus.iter().enumerate() {
        let (tx, rx) = channel::<Ctl>();
        inboxes.push(tx);
        let args = worker::WorkerArgs {
            core,
            cpu,
            fabric: fabric.clone(),
            inbox: rx,
            ready: ready_tx.clone(),
        };
        handles.push(
            std::thread::Builder::new()
                .name(format!("racer-w{core}"))
                .spawn(move || worker::worker_main::<H>(args, handler))?,
        );
    }
    drop(ready_tx);
    while ready_rx.recv().is_ok() {}

    let (tx, rx) = channel::<Job<H::Config>>();
    let cpu0 = cpus[0];
    let core = Core {
        ctl: Some(ctl),
        workers,
        disks: Vec::new(),
        vols: Vec::new(),
        file_used: vec![false; FILE_SLOTS as usize],
        dev_used: vec![false; MAX_DEVICES as usize],
        new_files: Vec::new(),
        new_devs: Vec::new(),
        declared_vols: Vec::new(),
    };
    // `Ctx` owns the live config versions, so it is built on the control thread
    // itself: `H::Config` is only `Sync`, never `Send`.
    let control = std::thread::Builder::new()
        .name("racer-ctl".into())
        .spawn(move || {
            // Park on an SMT sibling no worker owns, so no worker loses its CPU.
            if let Some(sib) = sys::sibling_of(cpu0) {
                let _ = sys::pin(sib);
            }
            let mut ctx = Ctx::<H::Config> {
                cfgr: Configurator {
                    core: std::cell::RefCell::new(core),
                },
                hub: Hub { inboxes, doorbell },
                versions: VecDeque::new(),
                next_ver: 1,
            };
            // `stop` posts a teardown job before dropping the sender, so teardown has
            // always run by the time the loop ends.
            while let Ok(job) = rx.recv() {
                job(&mut ctx);
            }
        })?;

    Ok(Runtime {
        inner: Arc::new(Inner {
            tx: Mutex::new(Some(tx)),
            control: Mutex::new(Some(control)),
            workers: Mutex::new(handles),
            down: AtomicBool::new(false),
        }),
    })
}

// ---------------------------------------------------------------------------
// Reconfiguration (control thread only)
// ---------------------------------------------------------------------------

fn reconcile<C: Sync + 'static, F>(ctx: &mut Ctx<C>, build: F) -> std::io::Result<()>
where
    F: FnOnce(&Configurator) -> std::io::Result<C>,
{
    {
        let mut c = ctx.cfgr.core.borrow_mut();
        c.new_files.clear();
        c.new_devs.clear();
        c.declared_vols.clear();
    }

    // 1. Build. Disks are opened and devices created (but not started) as they are
    //    declared, so a failure here is undone below.
    let cfg = match build(&ctx.cfgr) {
        Ok(c) => Box::new(c),
        Err(e) => {
            rollback(ctx);
            return Err(e);
        }
    };

    let (new_files, new_devs, retiring) = {
        let c = ctx.cfgr.core.borrow();
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
        ctx.hub
            .broadcast(|_, ack| Ctl::RegisterFiles(new_files.clone(), ack));
    }

    // 3. Devices that are going away stop accepting IO now; in-flight requests keep
    //    running against the old config, which still holds their `Volume`.
    for (slot, dev_id) in &retiring {
        let _ = ctx.cfgr.core.borrow_mut().ctl().stop_dev(*dev_id);
        ctx.hub
            .broadcast(|_, ack| Ctl::StopQueue { slot: *slot, ack });
    }

    // 4. Publish: the per-core cutover point.
    let ver = ctx.next_ver;
    ctx.next_ver += 1;
    let ptr: *const C = &*cfg;
    ctx.hub.broadcast(|_, ack| Ctl::Publish {
        ver,
        ptr: ptr as *const (),
        ack,
    });
    ctx.versions.push_back(Version { ver, cfg });
    // `retire_old` below drains every older version before returning, so a worker's
    // 4-slot guard table can never wrap.
    debug_assert!(ctx.versions.len() <= 2);

    // 5. Arm and start the new devices. The first request can only arrive after the
    //    config that describes it is already live on every core.
    for slot in &new_devs {
        let (dev_id, vol, q_ids, cpath) = {
            let c = ctx.cfgr.core.borrow();
            let v = c.vols.iter().find(|v| v.slot == *slot).unwrap();
            (
                v.dev_id,
                v.key,
                v.q_ids.clone(),
                ublk::char_dev_path(v.dev_id),
            )
        };
        let cdev = sys::open_flags(Path::new(&cpath), libc::O_RDWR | libc::O_CLOEXEC)?;
        let raw = cdev.as_raw_fd();
        ctx.hub.broadcast(|i, ack| Ctl::StartQueue {
            slot: *slot,
            vol,
            cfd: raw,
            depth: QUEUE_DEPTH,
            q_ids: q_ids[i].clone(),
            ack,
        });
        let mut c = ctx.cfgr.core.borrow_mut();
        c.ctl().start_dev(dev_id, std::process::id() as i32)?;
        if let Some(v) = c.vols.iter_mut().find(|v| v.slot == *slot) {
            v.cdev = Some(cdev);
        }
    }

    // 6. Retire everything older than the version we just published.
    retire_old(ctx, ver);
    Ok(())
}

/// Undo a failed build: close the fds it opened and delete the devices it created.
fn rollback<C>(ctx: &mut Ctx<C>) {
    let mut c = ctx.cfgr.core.borrow_mut();
    let new_files = std::mem::take(&mut c.new_files);
    for (slot, _) in new_files {
        c.file_used[slot as usize] = false;
        c.disks.retain(|d| d.slot != slot);
    }
    let new_devs = std::mem::take(&mut c.new_devs);
    for slot in new_devs {
        if let Some(pos) = c.vols.iter().position(|v| v.slot == slot) {
            let dev_id = c.vols[pos].dev_id;
            let _ = c.ctl().del_dev(dev_id);
            c.vols.remove(pos);
        }
        c.dev_used[slot as usize] = false;
    }
}

/// Drop configurations older than `keep`, once no core still holds a guard for them,
/// then reclaim any handle whose last reference went with them.
fn retire_old<C>(ctx: &mut Ctx<C>, keep: u32) {
    while let Some(front) = ctx.versions.front() {
        if front.ver >= keep {
            break;
        }
        let ver = front.ver;
        ctx.hub.broadcast(|_, ack| Ctl::Retire { ver, ack });
        ctx.versions.pop_front();
    }
    reclaim(ctx);
}

/// Free file slots and devices whose handles are gone. Nothing can be in flight against
/// them: the configuration that owned them has already been dropped.
fn reclaim<C>(ctx: &mut Ctx<C>) {
    let dead_files: Vec<u32> = {
        let mut c = ctx.cfgr.core.borrow_mut();
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
        ctx.hub
            .broadcast(|_, ack| Ctl::UnregisterFiles(dead_files.clone(), ack));
    }

    let mut c = ctx.cfgr.core.borrow_mut();
    let dead_vols: Vec<(u16, u32)> = c
        .vols
        .iter()
        .filter(|v| v.weak.strong_count() == 0)
        .map(|v| (v.slot, v.dev_id))
        .collect();
    for (slot, dev_id) in dead_vols {
        // DEL_DEV blocks until the last reference to the char device is gone, so
        // ours has to go first. The workers dropped theirs when the queue drained.
        if let Some(v) = c.vols.iter_mut().find(|v| v.slot == slot) {
            v.cdev = None;
        }
        let _ = c.ctl().del_dev(dev_id);
        c.dev_used[slot as usize] = false;
        c.vols.retain(|v| v.slot != slot);
    }
}

fn teardown<C>(ctx: &mut Ctx<C>) -> std::io::Result<()> {
    // Stop every live device, then let the workers drain.
    let live: Vec<(u16, u32)> = ctx
        .cfgr
        .core
        .borrow()
        .vols
        .iter()
        .map(|v| (v.slot, v.dev_id))
        .collect();
    for (slot, dev_id) in &live {
        let _ = ctx.cfgr.core.borrow_mut().ctl().stop_dev(*dev_id);
        ctx.hub
            .broadcast(|_, ack| Ctl::StopQueue { slot: *slot, ack });
    }

    // Retire every configuration, dropping the handles it held.
    let vers: Vec<u32> = ctx.versions.iter().map(|v| v.ver).collect();
    for ver in vers {
        ctx.hub.broadcast(|_, ack| Ctl::Retire { ver, ack });
    }
    ctx.versions.clear();

    ctx.hub.broadcast(|_, ack| Ctl::Shutdown(ack));

    let mut c = ctx.cfgr.core.borrow_mut();
    // Drop our char-device handles first: DEL_DEV waits for the last reference.
    for v in c.vols.iter_mut() {
        v.cdev = None;
    }
    for (_, dev_id) in live {
        let _ = c.ctl().del_dev(dev_id);
    }
    c.vols.clear();
    c.disks.clear();
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Seek, SeekFrom, Write};
    use std::sync::atomic::AtomicU64;

    struct Passthrough;

    struct Conf {
        store: Disk,
        /// Worker count, captured at build time; `Configurator` is the only place it
        /// is knowable off a worker thread.
        cores: usize,
        /// Never read: holding it is what keeps the device attached.
        #[allow(dead_code)]
        vol: Volume,
    }

    // The block map lives on the core that owns each lba, as the allocator's index
    // shards do: `handle` hops to look it up, then does the IO where the request
    // landed.
    thread_local! {
        static SEEN: std::cell::RefCell<std::collections::HashMap<u64, u64>> =
            std::cell::RefCell::new(std::collections::HashMap::new());
    }

    static HANDLER: Passthrough = Passthrough;
    static TICKS: AtomicU64 = AtomicU64::new(0);
    /// One bit per worker, so a test can tell "some core ticked" from "every core did".
    static TICKED: AtomicU64 = AtomicU64::new(0);
    /// Worker count, republished from a worker so the assertion sees a live value.
    static NCORES: AtomicU64 = AtomicU64::new(0);

    impl Handler for Passthrough {
        type Config = Conf;

        async fn handle(&'static self, cfg: Cfg<Conf>, req: Request) -> Result<(), Errno> {
            let lba = req.lba;
            let n = cfg.cores;
            // Fan the lookup out to two cores and take the first answer; the slow leg
            // is abandoned in flight, the paxos quorum pattern.
            let legs: [_; 2] = std::array::from_fn(|i| {
                let dst = (lba as usize + i) % n;
                on_core(dst, move || async move {
                    if i == 1 {
                        sleep(Duration::from_millis(2)).await;
                    }
                    Ok::<u64, Errno>(SEEN.with(|m| *m.borrow_mut().entry(lba).or_insert(lba * 4096)))
                })
            });
            let off = quorum(legs, 1)
                .await
                .into_iter()
                .flatten()
                .find_map(|r| r.ok())
                .ok_or(Errno::EIO)?;

            match req.op {
                Op::Read => cfg.store.read(off, req.buf).await,
                Op::Write => cfg.store.write(off, req.buf, Durability::Durable).await,
                Op::Discard => Ok(()),
            }
        }

        fn tick(&'static self, cfg: Cfg<Conf>, _now: Instant) {
            TICKS.fetch_add(1, Ordering::Relaxed);
            TICKED.fetch_or(1 << (core() % 64), Ordering::Relaxed);
            NCORES.store(cfg.cores as u64, Ordering::Relaxed);
        }
    }

    fn privileged() -> bool {
        std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open("/dev/ublk-control")
            .is_ok()
    }

    /// `core()` is only meaningful on a worker; off one it must fail loudly rather
    /// than hand back a plausible lie.
    #[test]
    fn core_id_off_worker_panics() {
        assert!(std::panic::catch_unwind(core).is_err());
    }

    /// Needs the real kernel seams: `sim` replaces disk submission, the timer and the
    /// clock with an event queue only a `Sim` drives, so a boot outside one never
    /// completes.
    #[cfg(not(feature = "sim"))]
    #[test]
    fn ublk_passthrough() {
        let _only = exclusive();
        if !privileged() {
            eprintln!("skipping: /dev/ublk-control is unavailable");
            return;
        }

        let backing = std::env::temp_dir().join("racer-e2e.img");
        {
            let f = std::fs::File::create(&backing).unwrap();
            f.set_len(64 << 20).unwrap();
        }

        let rt = start(&HANDLER).expect("start");
        // Only one runtime at a time.
        assert!(start(&HANDLER).is_err());

        let path = backing.clone();
        // The kernel picks the device id, so the config reports back where it landed.
        let found = Arc::new(Mutex::new(None));
        let out = found.clone();
        rt.reload(move |c| {
            let vol = c.volume(1, 32 << 20, false)?;
            *out.lock().unwrap() = Some(vol.path().to_path_buf());
            Ok(Conf {
                store: c.disk(&path, None, None)?,
                cores: c.cores(),
                vol,
            })
        })
        .expect("reload");

        // The device node appears as soon as START_DEV returns, but udev may lag.
        let dev = found.lock().unwrap().clone().expect("no volume declared");
        for _ in 0..100 {
            if dev.exists() {
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(dev.exists(), "no ublk block device appeared");

        let mut f = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(&dev)
            .expect("open ublkb");

        // Larger than one page, so the block layer splits it into several requests.
        let pattern: Vec<u8> = (0..32768u32).map(|i| (i % 251) as u8).collect();
        f.write_all(&pattern).unwrap();
        f.sync_all().unwrap();

        f.seek(SeekFrom::Start(0)).unwrap();
        let mut back = vec![0u8; pattern.len()];
        f.read_exact(&mut back).unwrap();
        assert_eq!(back, pattern, "data did not survive the round trip");

        // Concurrent IO from several threads, so requests land on both of each
        // worker's SMT-sibling queues.
        std::thread::scope(|s| {
            for t in 0..8u64 {
                let dev = dev.clone();
                s.spawn(move || {
                    use std::os::unix::fs::FileExt;
                    let f = std::fs::OpenOptions::new()
                        .read(true)
                        .write(true)
                        .open(&dev)
                        .unwrap();
                    let buf = vec![t as u8 + 1; 64 << 10];
                    let mut back = vec![0u8; buf.len()];
                    for r in 0..16u64 {
                        let off = (1 << 20) + (t * 16 + r) * (64 << 10);
                        f.write_at(&buf, off).unwrap();
                        f.read_at(&mut back, off).unwrap();
                        assert_eq!(back, buf, "thread {t} round {r}");
                    }
                });
            }
        });

        // Discard reaches the handler as its own op.
        let range = [0u64, 4096u64];
        let rc = unsafe {
            libc::ioctl(
                std::os::fd::AsRawFd::as_raw_fd(&f),
                0x1277, // BLKDISCARD
                range.as_ptr(),
            )
        };
        assert_eq!(
            rc,
            0,
            "BLKDISCARD failed: {}",
            std::io::Error::last_os_error()
        );
        drop(f);

        // Resizing a declared volume is a config error, and leaves the old one alone.
        let path = backing.clone();
        assert!(
            rt.reload(move |c| {
                Ok(Conf {
                    store: c.disk(&path, None, None)?,
                    cores: c.cores(),
                    vol: c.volume(1, 16 << 20, false)?,
                })
            })
            .is_err()
        );

        // Reload that drops the volume: drives stop, drain, retire and DEL_DEV.
        let path = backing.clone();
        rt.reload(move |c| {
            Ok(Conf {
                store: c.disk(&path, None, None)?,
                cores: c.cores(),
                vol: c.volume(2, 8 << 20, false)?,
            })
        })
        .expect("second reload");

        assert!(TICKS.load(Ordering::Relaxed) > 0, "tick never fired");

        // Every worker owes the handler a tick even with no IO anywhere: a parked
        // worker wakes on its own deadline. Background repair depends on this and it
        // is invisible from the client side, so assert it here.
        TICKED.store(0, Ordering::Relaxed);
        std::thread::sleep(Duration::from_millis(500));
        let n = NCORES.load(Ordering::Relaxed) as u32;
        let want = if n >= 64 { u64::MAX } else { (1u64 << n) - 1 };
        let got = TICKED.load(Ordering::Relaxed);
        assert_eq!(got, want, "idle workers missed their tick: {got:#x} of {want:#x}");

        rt.shutdown().expect("shutdown");

        // A runtime can be started again once the previous one is down.
        let rt = start(&HANDLER).expect("restart");
        rt.shutdown().expect("shutdown again");
        let _ = std::fs::remove_file(&backing);
    }
}
