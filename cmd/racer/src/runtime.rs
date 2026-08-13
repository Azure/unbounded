//! Racer runtime: ublk block devices served by one io_uring worker per physical core.
//!
//! Resources are declared, never opened ad hoc: [`Runtime::reload`] hands you a
//! [`Configurator`] whose handles stay registered on every worker until that config is
//! retired, so IO cannot name a file the kernel does not know about. Under `sim` no ublk
//! device exists and the tables below shrink.

mod exec;
mod hop;
mod io;
mod limit;
mod sys;
mod ublk;
mod worker;

use std::any::Any;
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

use crate::config::Class;
pub(crate) use io::{Buf, Disk, Durability, Export, PoolBuf, sleep};
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

/// The clock, which under simulation is virtual.
///
/// Anything that reads a clock outside a worker's own timing loop has to come through
/// here. A deadline set on the host clock and expired against virtual time is a
/// deadline that fires at whatever moment the simulation happens to have reached.
pub(crate) fn now() -> Instant {
    #[cfg(feature = "sim")]
    {
        crate::sim::clock()
    }
    #[cfg(not(feature = "sim"))]
    {
        Instant::now()
    }
}

use io::{DiskInner, ExportInner};
use worker::{Ack, Ctl, Doorbell};

/// Tags per ublk queue: in-flight requests per device per queue.
///
/// Buffer indices are dense over `(dev_slot, local_queue, tag)`, so their product is
/// bounded by `IORING_MAX_REG_BUFFERS` (16384): 256 devices at depth 16 costs 8192. A
/// worker serves two queues, so a device has 32 requests in flight per worker.
#[cfg(not(feature = "sim"))]
const QUEUE_DEPTH: u16 = 16;
/// A worker owns a physical core, so it serves both SMT siblings' hardware queues.
const QUEUES_PER_WORKER: usize = 2;
const TAGS_PER_DEV: u32 = QUEUES_PER_WORKER as u32 * QUEUE_DEPTH as u32;
/// Exported devices: one fabric device per universe plus one per configured device. A
/// slot costs one `DevSlot` row per worker plus `TAGS_PER_DEV` buffer indices, and the
/// kernel refuses more than `ublk_drv.ublks_max` devices (default 64).
#[cfg(not(feature = "sim"))]
const MAX_DEVICES: u16 = 256;

// Simulated dimensions: smallest per-worker tables the protocol fits; slabs run out sooner.
#[cfg(feature = "sim")]
const QUEUE_DEPTH: u16 = 4;
#[cfg(feature = "sim")]
const MAX_DEVICES: u16 = 16;

/// Largest single request the block layer may send: 4 MiB, one immutable huge page.
const MAX_IO_BYTES: usize = 4 << 20;
/// Registered file table: `0..MAX_DEVICES` are ublk char devices, the rest are disks.
#[cfg(not(feature = "sim"))]
const FILE_SLOTS: u32 = 1024;
#[cfg(feature = "sim")]
const FILE_SLOTS: u32 = 64;
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

/// One block-layer request. `dev` is the exported device's key, `lba` is in 4 KiB units,
/// and `buf` is the guest's own io_uring-registered pages, so IO never copies.
#[derive(Copy, Clone, Debug)]
pub struct Request {
    pub(crate) dev: u64,
    pub(crate) op: Op,
    pub(crate) lba: u64,
    pub(crate) buf: Buf,
}

impl Request {
    /// Copy `dst.len()` bytes of a write request's payload out of the guest at `off`.
    /// Escape hatch from [`Buf`]'s opacity, to stage a 4 KiB payload for checksum or
    /// parse. ublk's `USER_COPY` copies in the kernel via a plain `pread(2)` on the char
    /// device: io_uring rejects a fixed buffer there and would punt a plain one to io-wq.
    /// 4 MiB pages never take this path.
    pub(crate) fn load(&self, off: usize, dst: &mut [u8]) -> Result<(), Errno> {
        #[cfg(feature = "sim")]
        return crate::sim::copy_req(self.buf, off, dst.as_mut_ptr(), dst.len(), false);
        #[cfg(not(feature = "sim"))]
        worker::copy_req(
            self.buf.index as u32,
            off,
            dst.as_mut_ptr(),
            dst.len(),
            false,
        )
    }

    /// Copy `src` into a read request's payload at `off`. Mirror of [`Request::load`].
    pub(crate) fn store(&self, off: usize, src: &[u8]) -> Result<(), Errno> {
        #[cfg(feature = "sim")]
        return crate::sim::copy_req(self.buf, off, src.as_ptr() as *mut u8, src.len(), true);
        #[cfg(not(feature = "sim"))]
        worker::copy_req(
            self.buf.index as u32,
            off,
            src.as_ptr() as *mut u8,
            src.len(),
            true,
        )
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
    // `fabric` names its statuses out of these: nvmet's `blk_to_nvme_status()` maps only
    // four errnos; others reach the initiator as transport failures and cause failover.
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
/// `!Send`: the reference count is a plain per-core counter, which keeps reload off the
/// hot path. A version retires only once every core reports zero outstanding guards.
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

    /// Narrow the guard to one part of what it pins.
    ///
    /// The count keeps the whole configuration alive, so a borrow of any part of it is
    /// held up by exactly the same count; narrowing moves the count rather than taking a
    /// second one. The higher-ranked bound is what makes that sound: `f` has no lifetime
    /// of its own to return, so the only thing it can hand back is something it reached
    /// through the configuration it was given.
    pub fn map<T>(self, f: impl for<'a> FnOnce(&'a C) -> &'a T) -> Cfg<T> {
        let ptr: *const T = f(&self);
        let ver = self.ver;
        // The count transfers: no decrement here, and none was taken above.
        std::mem::forget(self);
        Cfg::new(ptr, ver)
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
    /// Disks, devices and peer tables the runtime keeps alive; built in
    /// [`Runtime::reload`].
    type Config: Sync + 'static;

    /// State one worker owns outright, reachable only through [`with_core`].
    ///
    /// Unlike [`Handler::Config`] this is never swapped: a reload replaces what a core
    /// reads, not the shards, slots and counters it owns. It reaches the runtime through
    /// [`Configurator::core_state`], while the first configuration is being built.
    type CoreState: Send + 'static;

    /// Serve one request. The returned future is stored in a preallocated slot, so it
    /// must be small; put cold paths behind `Box::pin(..).await`.
    fn handle(
        &'static self,
        cfg: Cfg<Self::Config>,
        req: Request,
    ) -> impl Future<Output = Result<(), Errno>> + 'static;

    /// Maintenance hook: runs on every core about every millisecond, idle or not, for
    /// per-core state. Runs on the worker thread, so it must not block.
    fn tick(&'static self, _cfg: Cfg<Self::Config>, _now: Instant) {}
}

/// A worker index, checked once against the runtime's worker count.
///
/// The only way to name a core. Holding one is not proof you are running on it:
/// [`with_core`] is what turns a `CoreId` into access to that core's state.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash, Debug)]
pub struct CoreId(u16);

impl CoreId {
    /// `None` if `i` is not a worker index. Callers derive `i` from an address or a
    /// consensus group, so this is the one place a mapping bug is caught.
    pub(crate) fn new(i: usize) -> Option<CoreId> {
        (i < cores()).then_some(CoreId(i as u16))
    }

    /// Unchecked: for the runtime naming a worker's own index.
    pub(crate) fn of(i: usize) -> CoreId {
        debug_assert!(i <= u16::MAX as usize);
        CoreId(i as u16)
    }

    pub(crate) fn index(self) -> usize {
        self.0 as usize
    }
}

/// The worker running this code.
// The subsystems get the core they are on from the `CoreCtx` they are handed, so only
// the runtime's own tests need to ask for it bare.
#[allow(dead_code)]
pub(crate) fn core_id() -> CoreId {
    CoreId::of(worker::core())
}

/// Workers in this runtime.
pub(crate) fn cores() -> usize {
    worker::cores()
}

/// The configuration this worker is running under, pinned until the guard is dropped.
///
/// [`Handler::handle`] and [`Handler::tick`] are handed one. This is for the code they
/// call that is not: a hop closure runs on the destination core, so it re-reads the live
/// configuration there rather than carrying the caller's borrow across a core boundary.
/// It sees whatever generation that core has cut over to, which is the same guarantee a
/// second read on the calling core would give.
pub(crate) fn config<H: Handler>() -> Cfg<H::Config> {
    worker::config::<H::Config>()
}

/// One core's half of the dataplane, for the length of one transaction.
///
/// Handed to the closure by [`with_core`] on the core that owns the state; never
/// constructed by callers. `'b` is invariant and appears in no output type, so a borrow of
/// a shard or of the configuration cannot outlive the transaction that took it. That is
/// what makes "the borrow ends before the await" a compile error rather than a comment.
pub struct CoreCtx<'b, H: Handler> {
    core: CoreId,
    cfg: &'b H::Config,
    state: &'b H::CoreState,
    _brand: PhantomData<fn(&'b ()) -> &'b ()>,
    _nosend: PhantomData<*const ()>,
}

impl<'b, H: Handler> CoreCtx<'b, H> {
    pub(crate) fn new(core: CoreId, cfg: &'b H::Config, state: &'b H::CoreState) -> Self {
        CoreCtx {
            core,
            cfg,
            state,
            _brand: PhantomData,
            _nosend: PhantomData,
        }
    }

    /// The worker this transaction is running on, and whose state [`Self::state`] is.
    pub fn core(&self) -> CoreId {
        self.core
    }

    pub fn cfg(&self) -> &'b H::Config {
        self.cfg
    }

    pub fn state(&self) -> &'b H::CoreState {
        self.state
    }
}

/// Run `f` on the worker that owns `dst`'s state and return its value.
///
/// The body is synchronous: it holds `dst`'s state for the whole transaction and cannot
/// await, so no other task observes a half-applied step and no configuration guard is
/// needed. On this core it runs inline. Otherwise the closure is copied into the
/// destination's ring and runs during its next drain, with no task-slab slot.
///
/// Dropping the returned future abandons the reply; a transaction already sent still runs.
pub(crate) fn with_core<H, F, T>(dst: CoreId, f: F) -> impl Future<Output = T>
where
    H: Handler,
    F: FnOnce(CoreCtx<'_, H>) -> T + Send + 'static,
    T: Send + 'static,
{
    hop::Call::<H, F, T>::new(dst.index(), f)
}

/// Run `f` against this worker's own state.
///
/// [`with_core`] for the core already running, without the future: for synchronous code
/// that only ever touches what is in front of it, and for the frames of an async one that
/// are between awaits. Same rule, and the reason it needs no guard: `f` cannot await.
pub(crate) fn here<H, T>(f: impl FnOnce(CoreCtx<'_, H>) -> T) -> T
where
    H: Handler,
{
    worker::with_core_ctx::<H, T>(f)
}

/// Run `f` on `core` and await its result there.
/// On this core the closure runs inline. Otherwise it is copied into the destination's
/// SPSC ring and polled there; only the result travels back. Dropping the returned future
/// abandons the call: the destination still runs, but its result is discarded.
pub(crate) fn on_core<F, Fut, T>(core: usize, f: F) -> impl Future<Output = T>
where
    F: FnOnce() -> Fut + Send + 'static,
    Fut: Future<Output = T> + 'static,
    T: Send + 'static,
{
    hop::Hop::<F, Fut, T>::new(core, f)
}

/// Run `fut` on this core to completion, detached: no handle and no result.
/// [`Handler::tick`] is synchronous, so maintenance that waits on I/O (`heal`'s
/// anti-entropy sweep) needs another home. The task sits in the same per-core slab as
/// remote futures, so it counts toward quiescence. Returns false if the slab is full,
/// having dropped `fut` unpolled.
pub(crate) fn spawn(fut: impl Future<Output = ()> + 'static) -> bool {
    hop::spawn(fut)
}

/// Run `fut`, giving up after `d`. `None` means it was still running and has been dropped.
///
/// Background work must not be able to wait forever. A detached maintenance task holds
/// whatever it borrowed for as long as it runs, and one of those things is the
/// configuration itself: a task parked on an await nothing will complete pins the version
/// it started under, and the reconfiguration that retires that version blocks behind it,
/// which takes the whole node out of the control plane's hands. Abandoning the task costs
/// one interval of progress; not abandoning it costs the node.
pub(crate) async fn deadline<F: Future>(fut: F, d: Duration) -> Option<F::Output> {
    let mut fut = std::pin::pin!(fut);
    let timer = worker::arm_deadline(now() + d);
    std::future::poll_fn(|cx| {
        if let Poll::Ready(v) = fut.as_mut().poll(cx) {
            return Poll::Ready(Some(v));
        }
        if timer.expired() {
            return Poll::Ready(None);
        }
        timer.watch(cx.waker());
        Poll::Pending
    })
    .await
}

// --- Fan-out / fan-in ---
// Futures are held inline (no allocation) and dropped in place, so they may be `!Unpin`.

/// Poll all `futs` concurrently until `need` of them succeed, then abandon the rest.
/// Resolves once `need` succeed or so many fail that `need` is unreachable. Slot `i`
/// holds `futs[i]`'s outcome, or `None` if it was still running at resolution.
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

// --- Configurator ---

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
    cdev: Option<std::os::fd::OwnedFd>,
}

struct Core {
    /// Absent in the simulator; used only on the control thread's reconfiguration path.
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
    /// Rows waiting to be handed to the workers, still owned so a failed build drops
    /// them. Erased here, and cast back against `H::CoreState` on the way out.
    core_state: Option<Box<dyn Any + Send>>,
}

impl Core {
    fn ctl(&mut self) -> &mut ublk::Control {
        self.ctl.as_mut().expect("no ublk control plane")
    }
}

/// Declares a configuration's resources; handles stay registered until it is retired.
pub struct Configurator {
    core: std::cell::RefCell<Core>,
}

#[cfg(feature = "sim")]
impl Configurator {
    /// A configurator with no kernel behind it: disks resolve in the simulator's table.
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
                core_state: None,
            }),
        }
    }
}

impl Configurator {
    /// Number of workers; configs are built on the control thread, where `core()` panics.
    pub(crate) fn cores(&self) -> usize {
        self.core.borrow().workers.len()
    }

    /// Hand over the state each worker will own, one row per worker in worker order.
    ///
    /// Offered here rather than derived from the configuration because a row is a cost of
    /// opening: a shard comes off the startup scan and nothing later can recover it. The
    /// runtime installs the rows once the configuration behind them is live, and before
    /// any worker takes traffic. A reload opens nothing, so it offers nothing, and a core
    /// keeps what it owns across one.
    pub(crate) fn core_state<S: Send + 'static>(&self, rows: Vec<S>) {
        let mut c = self.core.borrow_mut();
        assert_eq!(rows.len(), c.workers.len(), "one core state row per worker");
        assert!(c.core_state.is_none(), "core state offered twice");
        c.core_state = Some(Box::new(rows));
    }

    /// Take back what a build offered. The simulator installs its own workers, so it
    /// drains the rows here rather than through `reconcile`.
    #[cfg(feature = "sim")]
    pub(crate) fn take_core_state<S: Send + 'static>(&self) -> Vec<S> {
        *self
            .core
            .borrow_mut()
            .core_state
            .take()
            .expect("core state is offered while opening")
            .downcast::<Vec<S>>()
            .expect("core state rows are the handler's own")
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
        // The simulator names the slot directly: nothing to open, nothing to register.
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

    /// A block device this node exports.
    ///
    /// `key` is stable identity: re-declaring it keeps the same device and never
    /// interrupts IO. `minor` is the ublk device number asked of the kernel, so the export
    /// appears at `/dev/ublkb<minor>` and peers and mounts find it where the control plane
    /// said they would. `size` is in bytes, a nonzero multiple of 4 KiB, and neither it
    /// nor the minor nor `class` may change while the export lives.
    pub(crate) fn device(
        &self,
        key: u64,
        minor: u32,
        size: u64,
        class: Class,
    ) -> std::io::Result<Export> {
        let unit = if class == Class::Huge { 4 << 20 } else { 4096 };
        if !size.is_multiple_of(unit) || size == 0 {
            return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
        }
        self.declare(key, minor, size, ublk::params_for(size, class))
    }

    /// The node's fabric device: the one namespace peers issue against.
    /// Same machinery as [`Configurator::device`], different geometry: a fabric frame is
    /// 1 to 1024 blocks depending on the opcode, so it is not split on page boundaries.
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
        let slot = (0..MAX_DEVICES)
            .find(|s| !c.dev_used[*s as usize])
            .ok_or_else(|| std::io::Error::from_raw_os_error(libc::ENOSPC))?;

        // A simulated export is only a name: no kernel in between, so no device to create.
        #[cfg(feature = "sim")]
        let (dev_id, q_ids, path) = (
            minor,
            Vec::new(),
            PathBuf::from(format!("/sim/dev/{minor}")),
        );
        #[cfg(not(feature = "sim"))]
        let (dev_id, q_ids, path) = {
            // One hardware queue per logical CPU we own; blk-mq maps each to a core we
            // run on.
            let nq = c.workers.len() * QUEUES_PER_WORKER;
            let mut info = ublk::DevInfo::new(
                minor,
                nq as u16,
                QUEUE_DEPTH,
                ublk::F_AUTO_BUF_REG | ublk::F_USER_COPY | ublk::F_CMD_IOCTL_ENCODE,
            );
            add_dev(&mut c, &mut info)?;
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

/// Create the export at the minor it was named. The minor is not ours to choose: the
/// control plane published `/dev/ublkb<minor>` to whoever consumes it, so a device left at
/// that number by an instance of us that died is reclaimed rather than worked around. One
/// still being served is not: some other program has the number, and stopping it to take
/// the number would be worse than not exporting.
///
/// Reclaiming is a request, not a guarantee. The kernel frees a minor once the last
/// consumer closes the block device that used to be there, so a peer still holding the
/// export our predecessor left behind keeps the number for as long as it likes. We ask,
/// wait [`RECLAIM`] for the holders to let go, and then say plainly that they have not,
/// rather than parking in the kernel until they do.
#[cfg(not(feature = "sim"))]
fn add_dev(c: &mut Core, info: &mut ublk::DevInfo) -> std::io::Result<()> {
    let held = c.dev_used.iter().filter(|u| **u).count();
    let minor = info.dev_id;
    let taken = match c.ctl().add_dev(info) {
        Ok(_) => return Ok(()),
        Err(e) if e.raw_os_error() == Some(libc::EEXIST) => e,
        Err(e) => return Err(ublks_max_hint(e, held)),
    };
    // A dead device reports the pid that used to serve it, or none at all.
    let pid = c.ctl().dev_info(minor).map(|d| d.ublksrv_pid).unwrap_or(0);
    if pid > 0 && serving(pid) {
        return Err(std::io::Error::other(format!(
            "device {minor} is already exported by pid {pid}: {taken}"
        )));
    }
    c.ctl().del_dev_async(minor).map_err(|e| {
        std::io::Error::other(format!(
            "device {minor} is held by a dead export that will not go away: {e}"
        ))
    })?;
    let start = Instant::now();
    loop {
        match c.ctl().add_dev(info) {
            Err(e) if e.raw_os_error() == Some(libc::EEXIST) && start.elapsed() < RECLAIM => {
                std::thread::sleep(Duration::from_millis(20));
            }
            Err(e) if e.raw_os_error() == Some(libc::EEXIST) => {
                return Err(std::io::Error::other(format!(
                    "device {minor} is still open by a consumer of the export that died with \
                     our predecessor; it cannot be exported again until that consumer lets go"
                )));
            }
            Err(e) => return Err(ublks_max_hint(e, held)),
            Ok(_) => return Ok(()),
        }
    }
}

/// How long a minor left behind by a dead export is waited for. Long enough for the
/// kernel to finish a removal nobody is holding up, short enough that a start which
/// cannot have the number says so while the control plane is still watching.
#[cfg(not(feature = "sim"))]
const RECLAIM: Duration = Duration::from_secs(5);

/// Whether a pid is still around. `EPERM` counts: the process exists, it is simply not
/// ours to signal.
#[cfg(not(feature = "sim"))]
fn serving(pid: i32) -> bool {
    // SAFETY: signal 0 delivers nothing and only tests for the process.
    unsafe { libc::kill(pid, 0) == 0 || *libc::__errno_location() == libc::EPERM }
}

/// `ADD_DEV` fails once `ublk_drv.ublks_max` devices exist and the bare errno hides which
/// limit was hit; name the parameter, whose default of 64 is below [`MAX_DEVICES`].
#[cfg(not(feature = "sim"))]
fn ublks_max_hint(e: std::io::Error, held: usize) -> std::io::Error {
    const PARAM: &str = "/sys/module/ublk_drv/parameters/ublks_max";
    let max = std::fs::read_to_string(PARAM)
        .ok()
        .and_then(|s| s.trim().parse::<usize>().ok());
    match max {
        Some(m) => std::io::Error::other(format!(
            "ublk ADD_DEV failed with {held} devices already exported: {e}; {PARAM} is {m}, \
             raise it (ublk_drv.ublks_max=) to export more"
        )),
        None => e,
    }
}

/// Map each queue to a worker sharing its CPU, else the least loaded; `EINVAL` if full.
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

// --- Runtime ---

struct Version<C> {
    ver: u32,
    /// Owns the config while any core holds a guard; workers read it via the raw pointer.
    #[allow(dead_code)]
    cfg: Box<C>,
}

/// The control thread's channel to every worker: a queue plus a doorbell to wake it.
struct Hub {
    inboxes: Vec<Sender<Ctl>>,
    doorbell: Doorbell,
}

impl Hub {
    /// Broadcast one message and block until every worker drops its `Ack`, done or dead.
    fn broadcast(&mut self, mut make: impl FnMut(usize, Ack) -> Ctl) {
        let (tx, rx) = channel::<()>();
        for i in 0..self.inboxes.len() {
            if self.inboxes[i].send(make(i, tx.clone())).is_ok() {
                self.doorbell.wake(i);
            }
        }
        drop(tx);
        // Workers send `()` before dropping their `Ack`; the last drop ends the loop.
        while rx.recv().is_ok() {}
    }
}

/// Leaks one per-core state row per worker, checking on the way that the rows offered to
/// a configurator are the ones this handler will read back.
type CoreStateFn = fn(Box<dyn Any + Send>) -> Vec<*const ()>;

fn leak_core_state<S: Send + 'static>(rows: Box<dyn Any + Send>) -> Vec<*const ()> {
    // Leaked: a worker holds its row for the life of the process.
    rows.downcast::<Vec<S>>()
        .expect("core state rows are the handler's own")
        .into_iter()
        .map(|s| Box::into_raw(Box::new(s)) as *const ())
        .collect()
}

struct Ctx<C> {
    cfgr: Configurator,
    hub: Hub,
    versions: VecDeque<Version<C>>,
    next_ver: u32,
    /// Erased so `Ctx` stays free of `H`; the cast back happens here and nowhere else.
    core_state: CoreStateFn,
}

type Job<C> = Box<dyn FnOnce(&mut Ctx<C>) + Send>;

struct Inner<C> {
    tx: Mutex<Option<Sender<Job<C>>>>,
    control: Mutex<Option<std::thread::JoinHandle<()>>>,
    workers: Mutex<Vec<std::thread::JoinHandle<()>>>,
    down: AtomicBool,
}

/// Handle to a running runtime; dropping the last clone shuts the runtime down.
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
    /// `build` runs on the control thread. Anything not re-declared is torn down once the
    /// previous configuration is retired, after the last request that could still see it.
    pub fn reload<F>(&self, build: F) -> std::io::Result<()>
    where
        F: FnOnce(&Configurator) -> std::io::Result<C> + Send + 'static,
    {
        if self.inner.down.load(Ordering::Acquire) {
            return Err(std::io::Error::from_raw_os_error(libc::EINVAL));
        }
        let (tx, rx) = channel();
        post(
            &self.inner,
            Box::new(move |ctx| {
                let _ = tx.send(reconcile(ctx, build));
            }),
        )?;
        rx.recv()
            .map_err(|_| std::io::Error::from_raw_os_error(libc::EPIPE))?
    }

    /// Stop serving, tear down every device, wait for workers. Idempotent; runs on drop.
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
    let posted = post(
        inner,
        Box::new(move |ctx| {
            let _ = tx.send(teardown(ctx));
        }),
    );
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

/// `RUNNING` makes a second [`start`] fail, so runtime tests queue up. Hold this for the
/// life of the runtime; a panicking test poisons the lock and the next takes it anyway.
#[cfg(test)]
pub(crate) fn exclusive() -> std::sync::MutexGuard<'static, ()> {
    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    LOCK.lock().unwrap_or_else(|p| p.into_inner())
}

/// Boot the runtime: one pinned worker per physical core in the process affinity mask.
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
        core_state: None,
    };
    // `Ctx` is built on the control thread: `H::Config` is only `Sync`, never `Send`.
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
                core_state: leak_core_state::<H::CoreState>,
            };
            // `stop` posts teardown before dropping the sender, so it has run by loop exit.
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

// --- Reconfiguration (control thread only) ---

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

    // 1. Build. Disks open and devices are created, not started; failure is undone below.
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

    // 3. Retiring devices stop accepting IO; in-flight requests hold the old config.
    //
    // The order within each device is load bearing. STOP_DEV deletes the gendisk, and
    // deleting it waits for the request queue to freeze; the queue cannot freeze while
    // a consumer outside this process still has reads outstanding. The kernel aborts
    // those only when the last char-device reference goes away. So the workers drain
    // and drop theirs first, then we drop ours, and only then do we ask for the disk.
    // Asking first parks the configuration thread in `blk_mq_freeze_queue_wait` for as
    // long as the consumer keeps reading, and every later configuration is ignored in
    // silence because the reload never returns.
    for (slot, dev_id) in &retiring {
        ctx.hub
            .broadcast(|_, ack| Ctl::StopQueue { slot: *slot, ack });

        let mut c = ctx.cfgr.core.borrow_mut();
        if let Some(v) = c.vols.iter_mut().find(|v| v.slot == *slot) {
            v.cdev = None;
        }
        let _ = c.ctl().stop_dev(*dev_id);
    }

    // 4. Core state, if this build offered any.
    //
    // It goes in before the configuration rather than after. A core reads the two
    // together - `with_core_ctx` refuses to run without both - so the only thing
    // that decides whether a core can serve is which of them arrives last. Publish
    // first and there is a window between the two broadcasts in which a core holds
    // a configuration and no row, and the maintenance tick that fires in that
    // window walks straight into the assertion. State first has no such window:
    // a row without a configuration is unreachable, because nothing runs on a core
    // that has not been published to.
    let offered = ctx.cfgr.core.borrow_mut().core_state.take();
    if let Some(rows) = offered {
        let rows = (ctx.core_state)(rows);
        ctx.hub
            .broadcast(|i, ack| Ctl::InstallCoreState { ptr: rows[i], ack });
    }

    // 4b. Publish: the per-core cutover point.
    let ver = ctx.next_ver;
    ctx.next_ver += 1;
    let ptr: *const C = &*cfg;
    ctx.hub.broadcast(|_, ack| Ctl::Publish {
        ver,
        ptr: ptr as *const (),
        ack,
    });

    ctx.versions.push_back(Version { ver, cfg });
    // `retire_old` drains older versions, so a worker's 4-slot guard table cannot wrap.
    debug_assert!(ctx.versions.len() <= 2);

    // 5. Arm and start new devices; the config describing them is live on every core.
    for slot in &new_devs {
        let (dev_id, dev, q_ids, cpath) = {
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
            dev,
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

    retire_old(ctx, ver);
    Ok(())
}

/// Undo a failed build: close the fds it opened and delete the devices it created.
fn rollback<C>(ctx: &mut Ctx<C>) {
    let mut c = ctx.cfgr.core.borrow_mut();
    c.core_state = None;
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

/// Drop configurations older than `keep` once no core holds a guard, then reclaim.
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

/// Free file slots and devices whose handles are gone; the owning config is already
/// dropped, so nothing is in flight against them.
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

fn teardown<C>(ctx: &mut Ctx<C>) -> std::io::Result<()> {
    let live: Vec<(u16, u32)> = ctx
        .cfgr
        .core
        .borrow()
        .vols
        .iter()
        .map(|v| (v.slot, v.dev_id))
        .collect();
    for (slot, dev_id) in &live {
        // Same ordering as a reconfiguration: drain the workers, drop our char-device
        // handle, and only then delete the disk. A consumer with reads outstanding
        // would otherwise freeze the queue against us and hold shutdown open until it
        // gave up, and being killed for that leaves the export in a worse state.
        ctx.hub
            .broadcast(|_, ack| Ctl::StopQueue { slot: *slot, ack });

        let mut c = ctx.cfgr.core.borrow_mut();
        if let Some(v) = c.vols.iter_mut().find(|v| v.slot == *slot) {
            v.cdev = None;
        }
        let _ = c.ctl().stop_dev(*dev_id);
    }

    let vers: Vec<u32> = ctx.versions.iter().map(|v| v.ver).collect();
    for ver in vers {
        ctx.hub.broadcast(|_, ack| Ctl::Retire { ver, ack });
    }
    ctx.versions.clear();

    ctx.hub.broadcast(|_, ack| Ctl::Shutdown(ack));

    let mut c = ctx.cfgr.core.borrow_mut();
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::{Read, Seek, SeekFrom, Write};
    use std::sync::atomic::AtomicU64;

    struct Passthrough;

    struct Conf {
        store: Disk,
        /// Worker count at build time; only `Configurator` knows it off a worker thread.
        cores: usize,
        /// Never read: holding it keeps the device attached.
        #[allow(dead_code)]
        dev: Export,
    }

    // The block map is sharded by lba: each shard is its owner's `CoreState`.
    #[derive(Default)]
    struct Blocks {
        map: std::cell::RefCell<std::collections::HashMap<u64, u64>>,
    }

    /// Hand each worker its shard. Offered while the first configuration is built; a
    /// reload keeps what a core owns, so it offers nothing.
    fn shards(c: &Configurator) {
        c.core_state(
            (0..c.cores())
                .map(|_| Blocks::default())
                .collect::<Vec<_>>(),
        );
    }

    static HANDLER: Passthrough = Passthrough;
    static TICKS: AtomicU64 = AtomicU64::new(0);
    /// One bit per worker, so a test can tell "some core ticked" from "every core did".
    static TICKED: AtomicU64 = AtomicU64::new(0);
    /// Worker count, republished from a worker so the assertion sees a live value.
    static NCORES: AtomicU64 = AtomicU64::new(0);

    impl Handler for Passthrough {
        type Config = Conf;
        type CoreState = Blocks;

        async fn handle(&'static self, cfg: Cfg<Conf>, req: Request) -> Result<(), Errno> {
            let lba = req.lba;
            let n = cfg.cores;
            // Fan out to two cores and take the first answer; the slow leg is abandoned.
            let legs: [_; 2] = std::array::from_fn(|i| {
                let dst = CoreId::new((lba as usize + i) % n).expect("lba maps to a worker");
                on_core(dst.index(), move || async move {
                    if i == 1 {
                        sleep(Duration::from_millis(2)).await;
                    }
                    // Already on `dst`, so the transaction resolves inline; the point is
                    // that the shard is reachable only through one.
                    let off = with_core::<Passthrough, _, _>(dst, move |ctx| {
                        *ctx.state()
                            .map
                            .borrow_mut()
                            .entry(lba)
                            .or_insert(lba * 4096)
                    })
                    .await;
                    Ok::<u64, Errno>(off)
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
            TICKED.fetch_or(1 << (core_id().index() % 64), Ordering::Relaxed);
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

    /// `core()` is only meaningful on a worker; off one it must panic rather than lie.
    #[test]
    fn core_id_off_worker_panics() {
        assert!(std::panic::catch_unwind(core).is_err());
    }

    /// The minor this test asks for. Minors are host-wide, so every test that reaches
    /// the kernel owns a block of them the rest of the suite stays off.
    #[cfg(not(feature = "sim"))]
    const MINOR: u32 = 110;

    /// Needs the real kernel seams: under `sim` only a `Sim` drives the clock, so a
    /// boot outside one hangs.
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
        // The minor is asked for, not handed out, so the path is known before the reload.
        let found = Arc::new(Mutex::new(None));
        let out = found.clone();
        rt.reload(move |c| {
            shards(c);
            let dev = c.device(1, MINOR, 32 << 20, Class::Small)?;
            *out.lock().unwrap() = Some(dev.path().to_path_buf());
            Ok(Conf {
                store: c.disk(&path, None, None)?,
                cores: c.cores(),
                dev,
            })
        })
        .expect("reload");

        // The device node appears as soon as START_DEV returns, but udev may lag.
        let dev = found.lock().unwrap().clone().expect("no device declared");
        for _ in 0..100 {
            if dev.exists() {
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(dev.exists(), "no ublk block device appeared");
        assert_eq!(
            dev,
            PathBuf::from(format!("/dev/ublkb{MINOR}")),
            "the export landed on a minor other than the one asked for"
        );

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

        // Concurrent IO from several threads, so requests land on both sibling queues.
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

        // Resizing a declared device is a config error, and leaves the old one alone.
        let path = backing.clone();
        assert!(
            rt.reload(move |c| {
                Ok(Conf {
                    store: c.disk(&path, None, None)?,
                    cores: c.cores(),
                    dev: c.device(1, MINOR, 16 << 20, Class::Small)?,
                })
            })
            .is_err()
        );

        // So is moving it to another minor: the path is published, and a live export
        // cannot follow it.
        let path = backing.clone();
        assert!(
            rt.reload(move |c| {
                Ok(Conf {
                    store: c.disk(&path, None, None)?,
                    cores: c.cores(),
                    dev: c.device(1, MINOR + 2, 32 << 20, Class::Small)?,
                })
            })
            .is_err()
        );

        // And so is reshaping it: the block layer was told these limits once.
        let path = backing.clone();
        assert!(
            rt.reload(move |c| {
                Ok(Conf {
                    store: c.disk(&path, None, None)?,
                    cores: c.cores(),
                    dev: c.device(1, MINOR, 32 << 20, Class::Mixed)?,
                })
            })
            .is_err()
        );

        // Reload that drops the device: drives stop, drain, retire and DEL_DEV.
        let path = backing.clone();
        rt.reload(move |c| {
            Ok(Conf {
                store: c.disk(&path, None, None)?,
                cores: c.cores(),
                dev: c.device(2, MINOR + 1, 8 << 20, Class::Small)?,
            })
        })
        .expect("second reload");

        assert!(TICKS.load(Ordering::Relaxed) > 0, "tick never fired");

        // Every worker owes a tick even when idle; background repair depends on it.
        TICKED.store(0, Ordering::Relaxed);
        std::thread::sleep(Duration::from_millis(500));
        let n = NCORES.load(Ordering::Relaxed) as u32;
        let want = if n >= 64 { u64::MAX } else { (1u64 << n) - 1 };
        let got = TICKED.load(Ordering::Relaxed);
        assert_eq!(
            got, want,
            "idle workers missed their tick: {got:#x} of {want:#x}"
        );

        rt.shutdown().expect("shutdown");

        // A runtime can be started again once the previous one is down.
        let rt = start(&HANDLER).expect("restart");
        rt.shutdown().expect("shutdown again");
        let _ = std::fs::remove_file(&backing);
    }

    /// A crash leaves the minor behind: the kernel keeps the device, nothing serves it,
    /// and the control plane goes on publishing that number to consumers. Taking it back
    /// is the only way home short of a reboot, and it stops at a number some other
    /// program is still serving.
    #[cfg(not(feature = "sim"))]
    #[test]
    fn an_abandoned_minor_is_taken_back() {
        let _only = exclusive();
        if !privileged() {
            eprintln!("skipping: /dev/ublk-control is unavailable");
            return;
        }
        const LEFT: u32 = MINOR + 4;

        // What a crash leaves behind: a device the kernel holds and nobody serves.
        let mut ctl = ublk::Control::open().expect("control");
        let mut info = ublk::DevInfo::new(LEFT, 1, 8, ublk::F_CMD_IOCTL_ENCODE);
        ctl.add_dev(&mut info).expect("abandon a device");
        drop(ctl);

        let backing = std::env::temp_dir().join("racer-minor.img");
        {
            let f = std::fs::File::create(&backing).unwrap();
            f.set_len(16 << 20).unwrap();
        }

        let rt = start(&HANDLER).expect("start");
        let path = backing.clone();
        rt.reload(move |c| {
            shards(c);
            Ok(Conf {
                store: c.disk(&path, None, None)?,
                cores: c.cores(),
                dev: c.device(1, LEFT, 8 << 20, Class::Small)?,
            })
        })
        .expect("an abandoned minor is the node's own to retake");

        // Not so while it is being served: this process holds it now, so an export that
        // asks for the same number is refused, and told who has it.
        let path = backing.clone();
        let err = rt
            .reload(move |c| {
                Ok(Conf {
                    store: c.disk(&path, None, None)?,
                    cores: c.cores(),
                    dev: c.device(2, LEFT, 8 << 20, Class::Small)?,
                })
            })
            .expect_err("a live export keeps its minor");
        assert!(
            err.to_string()
                .contains(&format!("pid {}", std::process::id())),
            "a refusal has to name the holder: {err}"
        );

        rt.shutdown().expect("shutdown");
        let _ = std::fs::remove_file(&backing);
    }
}
