//! The worker: one io_uring per physical core, one thread, no locks on the hot path.
//!
//! A worker owns both SMT siblings' ublk hardware queues. `(dev slot, local queue, tag)`
//! flattens into one dense `tag id`: the registered buffer index, the request-slab index
//! and the `user_data` payload. `Local` is non-generic so it fits in a thread-local; the
//! request slab is generic over the handler's future, so it lives in `Worker` instead.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::future::Future;
use std::io;
use std::os::fd::{AsRawFd, BorrowedFd, RawFd};
use std::sync::Arc;
#[cfg(feature = "sim")]
use std::sync::mpsc::SyncSender;
use std::sync::mpsc::{Receiver, Sender};
use std::task::Context;
use std::time::{Duration, Instant};

use io_uring::{IoUring, cqueue, opcode, squeue, types};

use super::exec::{self, Exec};
use super::hop::{self, Fabric};
use super::io::{OpSlab, Pool};
use super::sys::{self, Mapping};
use super::ublk;
use super::{
    Cfg, CoreCtx, CoreId, Errno, FILE_SLOTS, Handler, MAX_DEVICES, MAX_IO_BYTES, Op, POOL_BUF_BASE,
    QUEUE_DEPTH, QUEUES_PER_WORKER, Request, TAGS_PER_DEV, TOTAL_BUF_SLOTS,
};

/// SQ entries; the CQ is 4x this (`Local::new`) so completion bursts never overflow.
#[cfg(not(feature = "sim"))]
const SQ_ENTRIES: u32 = 4096;
/// Minimal: the simulator builds a ring so `Local` has one shape, but never submits.
#[cfg(feature = "sim")]
const SQ_ENTRIES: u32 = 32;

/// `IORING_ENTER_GETEVENTS`; the crate's `submit()` omits it, so under `DEFER_TASKRUN`
/// completions would never be surfaced.
const ENTER_GETEVENTS: u32 = 1;

const SPIN_BUDGET: Duration = Duration::from_micros(50);
const POLL_BUDGET: Duration = Duration::from_micros(500);
/// How often `Handler::tick` fires on a busy worker.
const TICK_INTERVAL: Duration = Duration::from_millis(1);
/// Coarse wakeup so a parked worker still runs maintenance: repair (1 s) and cache decay.
const PARK_TIMEOUT: Duration = Duration::from_millis(100);

/// Op-slab utilisation above which completed requests are held back, throttling arrivals.
const COMMIT_DELAY_HIGH: f32 = 0.85;
const COMMIT_DELAY_LOW: f32 = 0.60;

// --- user_data codec: class:4 | seq:16 | slot:20 | payload:24 ---

const CLASS_OP: u64 = 0;
const CLASS_LINK: u64 = 1;
const CLASS_UBLK: u64 = 2;
const CLASS_DOORBELL: u64 = 3;
const CLASS_BUFREG: u64 = 4;

const fn ud(class: u64, seq: u64, slot: u64, payload: u64) -> u64 {
    (class << 60) | ((seq & 0xFFFF) << 44) | ((slot & 0xF_FFFF) << 24) | (payload & 0xFF_FFFF)
}

pub(super) fn ud_op(idx: u32, seq: u16) -> u64 {
    ud(CLASS_OP, seq as u64, idx as u64, 0)
}
pub(super) fn ud_link(idx: u32, seq: u16) -> u64 {
    ud(CLASS_LINK, seq as u64, idx as u64, 0)
}
fn ud_ublk(id: u32) -> u64 {
    ud(CLASS_UBLK, 0, 0, id as u64)
}
fn ud_bufreg(id: u32, unreg: bool) -> u64 {
    ud(CLASS_BUFREG, 0, unreg as u64, id as u64)
}
const UD_DOORBELL: u64 = ud(CLASS_DOORBELL, 0, 0, 0);
/// Completion of the doorbell SQE itself, on the *sending* ring. Ignored.
const UD_DOORBELL_SENT: u64 = ud(CLASS_DOORBELL, 0, 0, 1);

fn ud_class(u: u64) -> u64 {
    u >> 60
}
fn ud_gen(u: u64) -> u16 {
    ((u >> 44) & 0xFFFF) as u16
}
fn ud_slot(u: u64) -> u32 {
    ((u >> 24) & 0xF_FFFF) as u32
}
fn ud_payload(u: u64) -> u32 {
    (u & 0xFF_FFFF) as u32
}

// --- tag identity ---

/// Flattens `(dev slot, local queue, tag)`. Also the registered buffer index.
fn tag_id(slot: u16, lq: usize, tag: u16) -> u32 {
    slot as u32 * TAGS_PER_DEV + lq as u32 * QUEUE_DEPTH as u32 + tag as u32
}

fn tag_parts(id: u32) -> (u16, usize, u16) {
    (
        (id / TAGS_PER_DEV) as u16,
        (id % TAGS_PER_DEV) as usize / QUEUE_DEPTH as usize,
        (id % QUEUE_DEPTH as u32) as u16,
    )
}

// --- Control-plane messages ---

/// The control thread blocks on a broadcast until every worker acks or drops its ack.
pub(super) type Ack = Sender<()>;

pub(super) enum Ctl {
    /// Install `(slot, fd)` pairs into the registered file table.
    RegisterFiles(Vec<(u32, RawFd)>, Ack),
    /// Clear registered file slots.
    UnregisterFiles(Vec<u32>, Ack),
    /// Point this worker at a new config version.
    Publish {
        ver: u32,
        ptr: *const (),
        ack: Ack,
    },
    /// Ack once no live task on this worker still holds a guard for `ver`.
    Retire {
        ver: u32,
        ack: Ack,
    },
    /// Hand this worker the state it owns. Sent once, before it takes traffic; unlike
    /// `Publish` it is never swapped, so the state outlives every reload.
    InstallCoreState {
        ptr: *const (),
        ack: Ack,
    },
    /// Take ownership of this worker's ublk queues for a device and arm their fetches.
    StartQueue {
        slot: u16,
        dev: u64,
        cfd: RawFd,
        depth: u16,
        q_ids: Vec<u16>,
        ack: Ack,
    },
    /// Release a device's queues; acks once every tag has been reclaimed.
    StopQueue {
        slot: u16,
        ack: Ack,
    },
    Shutdown(Ack),
}

// SAFETY: only the receiving worker derefs `Publish`'s ptr; the config outlives `Retire`.
unsafe impl Send for Ctl {}

/// The control thread's channel for waking a worker blocked in `io_uring_enter`.
pub(super) struct Doorbell {
    ring: IoUring,
    fabric: Arc<Fabric>,
}

impl Doorbell {
    pub(super) fn new(fabric: Arc<Fabric>) -> io::Result<Doorbell> {
        Ok(Doorbell {
            ring: IoUring::builder().build(64)?,
            fabric,
        })
    }

    pub(super) fn wake(&mut self, dst: usize) {
        if push_doorbell(&self.ring, &self.fabric, dst) {
            let _ = self.ring.submitter().submit();
            // Keep the CQ from filling; these results never matter.
            self.ring.completion().for_each(drop);
        }
    }
}

/// Posts a `MSG_RING` onto `dst`'s ring if `dst` is asleep. The `SeqCst` fence pairs
/// with the sleeper publishing its state, so we never miss a wakeup.
fn push_doorbell(ring: &IoUring, fabric: &Fabric, dst: usize) -> bool {
    std::sync::atomic::fence(std::sync::atomic::Ordering::SeqCst);
    if !fabric.is_sleeping(dst) {
        return false;
    }
    let Some(fd) = fabric.ring_fd(dst) else {
        return false;
    };
    let e = opcode::MsgRingData::new(types::Fd(fd), 0, UD_DOORBELL, None)
        .build()
        .user_data(UD_DOORBELL_SENT);
    let mut sq = unsafe { ring.submission_shared() };
    unsafe { sq.push(&e) }.is_ok()
}

// --- Per-device state owned by a worker ---

const T_IDLE: u8 = 0;
const T_REG: u8 = 1;
const T_RUN: u8 = 2;
const T_UNREG: u8 = 3;

/// One ublk hardware queue; a worker owns up to `QUEUES_PER_WORKER` of these per device.
struct Queue {
    q_id: u16,
    descs: Mapping,
    /// Fetches currently owned by the kernel.
    armed: u32,
    /// Requests being served by the handler.
    inflight: u32,
    tag_state: Vec<u8>,
    tag_res: Vec<i32>,
    /// Request bytes captured at dispatch, so completion never re-reads the kernel desc.
    tag_bytes: Vec<u32>,
}

impl Queue {
    fn desc(&self, tag: u16) -> ublk::IoDesc {
        let base = self.descs.as_ptr() as *const ublk::IoDesc;
        // SAFETY: `descs` maps `depth` descriptors and `tag < depth`. The kernel writes
        // them from another context, so read once, volatile.
        unsafe { std::ptr::read_volatile(base.add(tag as usize)) }
    }
}

#[derive(Default)]
struct DevSlot {
    active: bool,
    stopping: bool,
    dev: u64,
    /// `/dev/ublkcN` for `USER_COPY` transfers. Control thread owns it; valid while active.
    cfd: RawFd,
    queues: [Option<Queue>; QUEUES_PER_WORKER],
    stop_ack: Option<Ack>,
}

impl DevSlot {
    fn idle(&self) -> bool {
        self.queues
            .iter()
            .flatten()
            .all(|q| q.armed == 0 && q.inflight == 0)
    }
}

// --- Local: the non-generic worker context reachable from any running future ---

pub(super) struct Local {
    pub(super) core: usize,
    pub(super) ring: IoUring,
    pub(super) fabric: Arc<Fabric>,
    pub(super) ops: OpSlab,
    pub(super) pool: Pool,
    pub(super) ready: exec::Ready,
    pub(super) hops: RefCell<hop::HopTasks>,
    pub(super) cells: RefCell<hop::Cells>,
    /// Hop messages rejected by a full destination ring; retried so no send ever fails.
    pub(super) hop_out: hop::Outbox,
    deferred: RefCell<VecDeque<Deferred>>,
    devs: RefCell<Vec<DevSlot>>,
    cfg_ptr: Cell<*const ()>,
    cfg_ver: Cell<u32>,
    /// This worker's `H::CoreState`, installed once before it takes traffic.
    state_ptr: Cell<*const ()>,
    /// One counter per config version; `reconcile` retires older ones, so four never wraps.
    guards: [Cell<u32>; 4],
    stop: Cell<bool>,
    /// Completed requests waiting for op-slab pressure to fall.
    commit_backlog: RefCell<VecDeque<(u32, i32)>>,
    throttled: Cell<bool>,
    pending_retire: RefCell<Vec<(u32, Ack)>>,
    /// Set while any device slot is draining, so the common turn skips the scan.
    draining: Cell<bool>,
}

enum Deferred {
    One(squeue::Entry),
    Pair(squeue::Entry, squeue::Entry),
}

thread_local! {
    static LOCAL: Cell<*const Local> = const { Cell::new(std::ptr::null()) };
}

/// Access the calling worker's runtime; panics off a worker thread.
pub(super) fn with_local<R>(f: impl FnOnce(&Local) -> R) -> R {
    let p = LOCAL.with(|c| c.get());
    assert!(!p.is_null(), "racer: not on a runtime worker thread");
    // SAFETY: the pointer is cleared before `Local` is dropped, on this same thread.
    f(unsafe { &*p })
}

/// Index of the worker running this code.
pub(crate) fn core() -> usize {
    with_local(|l| l.core)
}

/// Workers in this runtime.
#[allow(dead_code)]
pub(crate) fn cores() -> usize {
    with_local(|l| l.fabric.cores())
}

/// Build this worker's [`CoreCtx`] and run `f` under it.
///
/// Synchronous by construction: `f` cannot await, so the worker cannot process a `Retire`
/// while it runs and neither pointer can be pulled out from under it. That is why a core
/// transaction needs no configuration guard.
#[allow(dead_code)]
pub(super) fn with_core_ctx<H: Handler, R>(f: impl FnOnce(CoreCtx<'_, H>) -> R) -> R {
    with_local(|l| {
        let cfg = l.cfg_ptr.get();
        assert!(!cfg.is_null(), "racer: no configuration published");
        let state = l.state_ptr.get();
        assert!(!state.is_null(), "racer: no core state installed");
        // SAFETY: both are published by the control thread ahead of any traffic, and
        // outlive this body, which cannot yield.
        let ctx = unsafe {
            CoreCtx::new(
                CoreId::of(l.core),
                &*(cfg as *const H::Config),
                &*(state as *const H::CoreState),
            )
        };
        f(ctx)
    })
}

/// Queues a hop message for `dst`, deferring it if the ring is momentarily full.
pub(super) fn send_hop(dst: usize, msg: hop::Msg) {
    with_local(|l| l.send_hop(dst, msg));
}

/// Move one request's payload between the guest and our own memory (ublk USER_COPY).
///
/// Synchronous: io_uring rejects a registered buffer against the ublk char device and
/// punts a plain one to io-wq. The kernel `memcpy` over pinned pages never blocks.
pub(super) fn copy_req(
    id: u32,
    off: usize,
    ptr: *mut u8,
    len: usize,
    store: bool,
) -> Result<(), Errno> {
    let (slot, lq, tag) = tag_parts(id);
    let found = with_local(|l| {
        let devs = l.devs.borrow();
        let d = &devs[slot as usize];
        d.queues[lq].as_ref().map(|q| (d.cfd, q.q_id))
    });
    let Some((fd, q_id)) = found else {
        return Err(Errno::EIO);
    };
    let pos = ublk::buf_offset(q_id, tag, off) as i64;
    let n = unsafe {
        if store {
            libc::pwrite(fd, ptr as *const libc::c_void, len, pos)
        } else {
            libc::pread(fd, ptr as *mut libc::c_void, len, pos)
        }
    };
    if n < 0 {
        return Err(Errno::from_raw(
            io::Error::last_os_error()
                .raw_os_error()
                .unwrap_or(libc::EIO),
        ));
    }
    if n as usize != len {
        return Err(Errno::EIO);
    }
    Ok(())
}

pub(super) fn poll_hop_task(id: u32) {
    // The slab is preallocated, so these pointers stay valid while the job runs.
    let Some((vt, data)) = with_local(|l| {
        let h = l.hops.borrow();
        h.vt(id).map(|vt| (vt, h.data_ptr(id)))
    }) else {
        // Stale wake for a job that already finished; `Exec::poll` ignores these too.
        return;
    };
    let w = exec::waker_for(exec::KIND_HOP | id);
    let mut cx = Context::from_waker(&w);
    if unsafe { vt.poll(data, &mut cx) }.is_ready() {
        // The reply, if any, is already posted.
        unsafe { vt.drop_in_place(data) };
        with_local(|l| l.hops.borrow_mut().release(id));
    }
}

impl Local {
    fn new(core: usize, fabric: Arc<Fabric>) -> io::Result<Self> {
        let ring = IoUring::builder()
            .setup_single_issuer()
            .setup_defer_taskrun()
            .setup_taskrun_flag()
            .setup_submit_all()
            .setup_cqsize(SQ_ENTRIES * 4)
            .build(SQ_ENTRIES)?;

        let pool = Pool::new()?;
        // Registering pins every pool page: too costly in sim, which never submits SQEs.
        #[cfg(not(feature = "sim"))]
        {
            let s = ring.submitter();
            s.register_files_sparse(FILE_SLOTS)?;
            s.register_buffers_sparse(TOTAL_BUF_SLOTS)?;
            let iov = pool.iovecs();
            unsafe { s.register_buffers_update(POOL_BUF_BASE, &iov, None)? };
        }

        Ok(Local {
            core,
            ring,
            fabric,
            ops: OpSlab::new(),
            pool,
            ready: exec::Ready::new(1024),
            hops: RefCell::new(hop::HopTasks::new()),
            cells: RefCell::new(hop::Cells::new()),
            hop_out: RefCell::new(VecDeque::new()),
            deferred: RefCell::new(VecDeque::new()),
            devs: RefCell::new((0..MAX_DEVICES).map(|_| DevSlot::default()).collect()),
            cfg_ptr: Cell::new(std::ptr::null()),
            cfg_ver: Cell::new(0),
            state_ptr: Cell::new(std::ptr::null()),
            guards: [const { Cell::new(0) }; 4],
            stop: Cell::new(false),
            commit_backlog: RefCell::new(VecDeque::new()),
            throttled: Cell::new(false),
            pending_retire: RefCell::new(Vec::new()),
            draining: Cell::new(false),
        })
    }

    // -- submission ---------------------------------------------------------

    pub(super) fn push(&self, e: squeue::Entry) {
        let mut sq = unsafe { self.ring.submission_shared() };
        if unsafe { sq.push(&e) }.is_err() {
            drop(sq);
            self.deferred.borrow_mut().push_back(Deferred::One(e));
        }
    }

    /// A linked pair must land in one submission or the kernel severs the link; defer both.
    pub(super) fn push_linked(&self, a: squeue::Entry, b: squeue::Entry) {
        let mut sq = unsafe { self.ring.submission_shared() };
        if sq.capacity() - sq.len() >= 2 {
            unsafe {
                let _ = sq.push(&a);
                let _ = sq.push(&b);
            }
        } else {
            drop(sq);
            self.deferred.borrow_mut().push_back(Deferred::Pair(a, b));
        }
    }

    fn flush_deferred(&self) -> usize {
        let mut n = 0;
        loop {
            let mut d = self.deferred.borrow_mut();
            let Some(front) = d.front() else { break };
            let mut sq = unsafe { self.ring.submission_shared() };
            let need = match front {
                Deferred::One(_) => 1,
                Deferred::Pair(..) => 2,
            };
            if sq.capacity() - sq.len() < need {
                break;
            }
            match d.pop_front().unwrap() {
                Deferred::One(e) => unsafe {
                    let _ = sq.push(&e);
                },
                Deferred::Pair(a, b) => unsafe {
                    let _ = sq.push(&a);
                    let _ = sq.push(&b);
                },
            }
            n += 1;
        }
        n
    }

    fn submit(&self) {
        // An empty submission still costs a full `io_uring_enter`; only enter with SQEs.
        if unsafe { self.ring.submission_shared() }.is_empty() {
            return;
        }
        let _ = self.ring.submitter().submit();
    }

    fn reap(&self, out: &mut Vec<cqueue::Entry>) {
        if unsafe { self.ring.submission_shared() }.taskrun() {
            let _ = unsafe {
                self.ring
                    .submitter()
                    .enter::<()>(0, 0, ENTER_GETEVENTS, None)
            };
        }
        let mut cq = unsafe { self.ring.completion_shared() };
        cq.sync();
        out.extend(&mut cq);
    }

    // -- hops ---------------------------------------------------------------

    fn send_hop(&self, dst: usize, msg: hop::Msg) {
        // Preserve FIFO with anything already deferred.
        if !self.hop_out.borrow().is_empty() {
            self.hop_out.borrow_mut().push_back((dst as u16, msg));
            return;
        }
        match self.fabric.send(self.core, dst, msg) {
            None => {
                if push_doorbell(&self.ring, &self.fabric, dst) {
                    self.submit();
                }
            }
            Some(msg) => self.hop_out.borrow_mut().push_back((dst as u16, msg)),
        }
    }

    fn flush_hops(&self) -> usize {
        let mut n = 0;
        while let Some((dst, msg)) = self.hop_out.borrow_mut().pop_front() {
            if let Some(msg) = self.fabric.send(self.core, dst as usize, msg) {
                self.hop_out.borrow_mut().push_front((dst, msg));
                break;
            }
            if push_doorbell(&self.ring, &self.fabric, dst as usize) {
                self.submit();
            }
            n += 1;
        }
        n
    }

    // -- config guards ------------------------------------------------------

    pub(super) fn guard_inc(&self, ver: u32) {
        let c = &self.guards[(ver % 4) as usize];
        c.set(c.get() + 1);
    }

    pub(super) fn guard_dec(&self, ver: u32) {
        let c = &self.guards[(ver % 4) as usize];
        c.set(c.get() - 1);
    }

    pub(super) fn cfg<C>(&self) -> Cfg<C> {
        let ptr = self.cfg_ptr.get();
        assert!(!ptr.is_null(), "racer: no configuration published");
        let ver = self.cfg_ver.get();
        self.guard_inc(ver);
        Cfg::new(ptr as *const C, ver)
    }

    fn have_cfg(&self) -> bool {
        !self.cfg_ptr.get().is_null()
    }

    // -- ublk ---------------------------------------------------------------

    fn with_queue<R>(&self, id: u32, f: impl FnOnce(&mut Queue) -> R) -> Option<R> {
        let (slot, lq, _) = tag_parts(id);
        let mut devs = self.devs.borrow_mut();
        devs[slot as usize].queues[lq].as_mut().map(f)
    }

    /// Arm one tag: `FETCH_REQ` for a fresh tag, `COMMIT_AND_FETCH_REQ` to complete and
    /// take the next. Both carry the auto-buf-reg index so bio pages appear in our table.
    fn arm(&self, id: u32, cmd_op: u32, result: i32) {
        let (slot, lq, tag) = tag_parts(id);
        let q_id = {
            let mut devs = self.devs.borrow_mut();
            let d = &mut devs[slot as usize];
            let Some(q) = d.queues[lq].as_mut().filter(|_| d.active) else {
                return;
            };
            q.armed += 1;
            q.q_id
        };
        let cmd = ublk::IoCmd {
            q_id,
            tag,
            result,
            addr: 0,
        };
        let e = opcode::UringCmd16::new(types::Fixed(slot as u32), cmd_op)
            .cmd(cmd.encode())
            .addr(Some(ublk::auto_buf_reg(
                id as u16,
                ublk::AUTO_BUF_REG_FALLBACK,
            )))
            .build()
            .user_data(ud_ublk(id));
        self.push(e);
    }

    /// `UBLK_IO_F_NEED_REG_BUF` fallback: register or unregister the buffer by hand.
    fn buf_reg(&self, id: u32, unreg: bool) {
        let (slot, _, tag) = tag_parts(id);
        let Some(q_id) = self.with_queue(id, |q| q.q_id) else {
            return;
        };
        let cmd = ublk::IoCmd {
            q_id,
            tag,
            result: 0,
            addr: id as u64,
        };
        let op = if unreg {
            ublk::IO_UNREGISTER_IO_BUF
        } else {
            ublk::IO_REGISTER_IO_BUF
        };
        let e = opcode::UringCmd16::new(types::Fixed(slot as u32), op)
            .cmd(cmd.encode())
            .build()
            .user_data(ud_bufreg(id, unreg));
        self.push(e);
    }

    /// Hand a finished request back to the kernel, subject to the commit delay.
    fn commit(&self, id: u32, res: i32) {
        // Commit rearms the tag on the next request's pages; a live op would go stale.
        debug_assert!(
            !self.ops.tag_busy(id),
            "racer: committing tag {id} while an op still references its buffer"
        );
        if self.throttled.get() {
            self.commit_backlog.borrow_mut().push_back((id, res));
            return;
        }
        self.arm(id, ublk::IO_COMMIT_AND_FETCH_REQ, res);
    }

    fn update_throttle(&self) {
        let u = self.ops.utilization();
        if self.throttled.get() {
            if u < COMMIT_DELAY_LOW {
                self.throttled.set(false);
            }
        } else if u > COMMIT_DELAY_HIGH {
            self.throttled.set(true);
        }
    }

    fn drain_commit_backlog(&self) -> usize {
        if self.throttled.get() {
            return 0;
        }
        let mut n = 0;
        while let Some((id, res)) = self.commit_backlog.borrow_mut().pop_front() {
            self.arm(id, ublk::IO_COMMIT_AND_FETCH_REQ, res);
            n += 1;
        }
        n
    }
}

// --- The loop ---

pub(super) struct WorkerArgs {
    pub(super) core: usize,
    pub(super) cpu: usize,
    pub(super) fabric: Arc<Fabric>,
    pub(super) inbox: Receiver<Ctl>,
    pub(super) ready: Ack,
}

/// A panicking worker leaves the ring, op slab and ublk queue unrecoverable, so abort.
struct AbortOnPanic(usize);

impl Drop for AbortOnPanic {
    fn drop(&mut self) {
        if std::thread::panicking() {
            eprintln!("racer: worker {} panicked; aborting", self.0);
            std::process::abort();
        }
    }
}

/// The generic half of a worker: the request slab and the control inbox.
struct Worker<'a, H: Handler, F> {
    l: &'a Local,
    exec: Exec<H, F>,
    inbox: Receiver<Ctl>,
    staged: VecDeque<Ctl>,
    last_tick: Instant,
}

pub(super) fn worker_main<H: Handler>(args: WorkerArgs, handler: &'static H) {
    let _guard = AbortOnPanic(args.core);
    let _ = sys::pin(args.cpu);
    let cores = args.fabric.cores();
    let l = match Local::new(args.core, args.fabric.clone()) {
        Ok(l) => l,
        Err(e) => {
            eprintln!("racer: worker {} failed to start: {e}", args.core);
            return;
        }
    };
    args.fabric.publish(args.core, l.ring.as_raw_fd());
    LOCAL.with(|c| c.set(&l as *const Local));

    // Inferring `F`, the anonymous `H::handle` future type, keeps request futures unboxed.
    let mut w = Worker {
        l: &l,
        exec: Exec::new(
            handler,
            H::handle,
            (MAX_DEVICES as u32 * TAGS_PER_DEV) as usize,
        ),
        inbox: args.inbox,
        staged: VecDeque::new(),
        last_tick: Instant::now(),
    };
    if args.core == 0 {
        // The slab holds `MAX_DEVICES * TAGS_PER_DEV` of these, so growth matters.
        eprintln!(
            "racer: {cores} workers, request future is {} bytes",
            w.exec.future_size()
        );
    }
    drop(args.ready);

    w.run();

    // The slab must die before the thread-local: futures drop `Cfg` guards into `Local`.
    drop(w);
    LOCAL.with(|c| c.set(std::ptr::null()));
}

impl<H: Handler, F: Future<Output = Result<(), Errno>>> Worker<'_, H, F> {
    fn run(&mut self) {
        let mut cqes: Vec<cqueue::Entry> = Vec::with_capacity(1024);
        let mut clock = Instant::now();
        let mut idle_since = clock;
        let mut turn: u32 = 0;

        loop {
            let l = self.l;
            let mut work = 0usize;

            cqes.clear();
            l.reap(&mut cqes);
            work += cqes.len();
            for cqe in cqes.drain(..) {
                self.handle_cqe(cqe);
            }

            work += l.fabric.drain(l.core);
            work += l.flush_hops();

            while let Some(id) = l.ready.pop() {
                work += 1;
                if exec::is_hop(id) {
                    poll_hop_task(exec::slot_of(id));
                } else if let Some(res) = self.exec.poll(id) {
                    self.finish_request(id, res);
                }
            }

            work += l.flush_deferred();
            l.update_throttle();
            work += l.drain_commit_backlog();

            work += self.maintenance(clock);

            l.submit();

            if l.stop.get() && self.quiesced() {
                break;
            }

            // Reading the clock is a vdso call: re-read only after work or every 64th turn.
            turn = turn.wrapping_add(1);
            if work > 0 || turn.is_multiple_of(64) {
                clock = Instant::now();
            }
            if work > 0 {
                idle_since = clock;
                continue;
            }
            let idle = clock.saturating_duration_since(idle_since);
            if idle < SPIN_BUDGET {
                std::hint::spin_loop();
            } else if idle < POLL_BUDGET {
                unsafe { libc::sched_yield() };
            } else {
                self.park();
                clock = Instant::now();
            }
        }
    }

    fn quiesced(&self) -> bool {
        let l = self.l;
        self.exec.live_count() == 0
            && l.ops.inflight() == 0
            && l.ready.len() == 0
            && l.hops.borrow().live() == 0
            && l.hop_out.borrow().is_empty()
            && l.devs.borrow().iter().all(|d| !d.active)
            && self.staged.is_empty()
    }

    /// Dekker-style park: publish `Sleeping`, re-check every inbox, then block until a
    /// completion or `PARK_TIMEOUT`. A producer that missed the change already enqueued,
    /// so the re-check sees it; one that sees `Sleeping` rings a doorbell. `ETIME` retries.
    fn park(&mut self) {
        let l = self.l;
        l.fabric.set_sleeping(l.core);
        if l.fabric.pending(l.core) == 0 && l.ready.len() == 0 {
            match self.inbox.try_recv() {
                Ok(m) => self.staged.push_back(m),
                Err(_) => {
                    let ts = types::Timespec::new()
                        .sec(PARK_TIMEOUT.as_secs())
                        .nsec(PARK_TIMEOUT.subsec_nanos());
                    let args = types::SubmitArgs::new().timespec(&ts);
                    let _ = l.ring.submitter().submit_with_args(1, &args);
                }
            }
        }
        l.fabric.set_running(l.core);
    }

    // -- completions --------------------------------------------------------

    fn handle_cqe(&mut self, cqe: cqueue::Entry) {
        let l = self.l;
        let u = cqe.user_data();
        match ud_class(u) {
            CLASS_OP => l
                .ops
                .complete(&l.pool, ud_slot(u), ud_gen(u), cqe.result(), false),
            CLASS_LINK => l
                .ops
                .complete(&l.pool, ud_slot(u), ud_gen(u), cqe.result(), true),
            CLASS_DOORBELL => {}
            CLASS_UBLK => self.ublk_cqe(ud_payload(u), cqe.result()),
            CLASS_BUFREG => self.bufreg_cqe(ud_payload(u), ud_slot(u) == 1, cqe.result()),
            _ => debug_assert!(false, "racer: unknown user_data class"),
        }
    }

    fn ublk_cqe(&mut self, id: u32, res: i32) {
        let (slot, lq, _) = tag_parts(id);
        {
            let mut devs = self.l.devs.borrow_mut();
            let d = &mut devs[slot as usize];
            let Some(q) = d.queues[lq].as_mut() else {
                return;
            };
            q.armed -= 1;
            if res != ublk::IO_RES_OK || d.stopping {
                // -ENODEV on stop or any terminal error: the tag is ours again, no re-arm.
                return;
            }
        }
        self.start_request(id);
    }

    fn start_request(&mut self, id: u32) {
        let (slot, lq, tag) = tag_parts(id);
        let (desc, dev) = {
            let devs = self.l.devs.borrow();
            let d = &devs[slot as usize];
            match d.queues[lq].as_ref() {
                Some(q) => (q.desc(tag), d.dev),
                None => return,
            }
        };

        // Auto buffer registration failed; register by hand before dispatching.
        if desc.flags() & ublk::IO_F_NEED_REG_BUF != 0 {
            self.l.with_queue(id, |q| q.tag_state[tag as usize] = T_REG);
            self.l.buf_reg(id, false);
            return;
        }
        self.l.with_queue(id, |q| q.tag_state[tag as usize] = T_RUN);
        self.dispatch(id, dev, desc);
    }

    fn dispatch(&mut self, id: u32, dev: u64, desc: ublk::IoDesc) {
        let (_, _, tag) = tag_parts(id);
        let bytes = desc.nr_sectors * 512;
        debug_assert!(bytes as usize <= MAX_IO_BYTES);
        self.l.with_queue(id, |q| {
            q.inflight += 1;
            q.tag_bytes[tag as usize] = bytes;
        });

        let op = match desc.op() {
            ublk::OP_READ => Op::Read,
            ublk::OP_WRITE => Op::Write,
            ublk::OP_DISCARD => Op::Discard,
            _ => return self.finish_request(id, Err(Errno::EOPNOTSUPP)),
        };
        let req = Request {
            dev,
            op,
            lba: desc.start_sector / 8,
            buf: super::io::req_buf(id as u16, bytes),
        };
        let cfg = self.l.cfg::<H::Config>();
        if let Some(res) = self.exec.start(id, cfg, req) {
            self.finish_request(id, res);
        }
    }

    fn bufreg_cqe(&mut self, id: u32, unreg: bool, res: i32) {
        let (slot, lq, tag) = tag_parts(id);
        if unreg {
            let stored = self
                .l
                .with_queue(id, |q| {
                    q.tag_state[tag as usize] = T_IDLE;
                    q.tag_res[tag as usize]
                })
                .unwrap_or(0);
            self.l.commit(id, stored);
            return;
        }
        if res < 0 {
            self.l
                .with_queue(id, |q| q.tag_state[tag as usize] = T_IDLE);
            self.l.commit(id, res);
            return;
        }
        let (desc, dev) = {
            let devs = self.l.devs.borrow();
            let d = &devs[slot as usize];
            match d.queues[lq].as_ref() {
                Some(q) => (q.desc(tag), d.dev),
                None => return,
            }
        };
        self.l.with_queue(id, |q| q.tag_state[tag as usize] = T_RUN);
        self.dispatch(id, dev, desc);
    }

    fn finish_request(&mut self, id: u32, res: Result<(), Errno>) {
        let (_, _, tag) = tag_parts(id);
        let Some((bytes, needs_unreg)) = self.l.with_queue(id, |q| {
            q.inflight -= 1;
            let needs_unreg = q.tag_state[tag as usize] == T_UNREG
                || (q.tag_state[tag as usize] == T_RUN
                    && q.desc(tag).flags() & ublk::IO_F_NEED_REG_BUF != 0);
            (q.tag_bytes[tag as usize] as i32, needs_unreg)
        }) else {
            return;
        };
        let result = match res {
            Ok(()) => bytes,
            Err(e) => -e.raw(),
        };
        if needs_unreg {
            self.l.with_queue(id, |q| {
                q.tag_state[tag as usize] = T_UNREG;
                q.tag_res[tag as usize] = result;
            });
            self.l.buf_reg(id, true);
            return;
        }
        self.l
            .with_queue(id, |q| q.tag_state[tag as usize] = T_IDLE);
        self.l.commit(id, result);
    }

    // -- control ------------------------------------------------------------

    fn maintenance(&mut self, now: Instant) -> usize {
        let l = self.l;
        let mut n = 0;
        while let Some(m) = self.staged.pop_front() {
            n += 1;
            self.apply_ctl(m);
        }
        while let Ok(m) = self.inbox.try_recv() {
            n += 1;
            self.apply_ctl(m);
        }

        // Parked retirements: ack once nothing on this core still holds the version.
        if !l.pending_retire.borrow().is_empty() {
            l.pending_retire.borrow_mut().retain(|(ver, ack)| {
                if l.guards[(*ver % 4) as usize].get() == 0 {
                    let _ = ack.send(());
                    n += 1;
                    false
                } else {
                    true
                }
            });
        }

        // Devices finishing their drain: release the queues and the char-dev file slot.
        if l.draining.get() {
            let mut left = false;
            for slot in 0..MAX_DEVICES {
                let ack = {
                    let mut devs = l.devs.borrow_mut();
                    let d = &mut devs[slot as usize];
                    if !d.stopping {
                        continue;
                    }
                    if !d.idle() {
                        left = true;
                        continue;
                    }
                    d.active = false;
                    d.stopping = false;
                    d.queues = Default::default();
                    d.stop_ack.take()
                };
                let _ = l.ring.submitter().register_files_update(slot as u32, &[-1]);
                if let Some(a) = ack {
                    let _ = a.send(());
                    n += 1;
                }
            }
            l.draining.set(left);
        }

        if l.have_cfg() && now.saturating_duration_since(self.last_tick) >= TICK_INTERVAL {
            self.last_tick = now;
            H::tick(self.exec.handler(), l.cfg::<H::Config>(), now);
        }
        n
    }

    fn apply_ctl(&mut self, m: Ctl) {
        let l = self.l;
        match m {
            Ctl::RegisterFiles(items, ack) => {
                for (slot, fd) in items {
                    let _ = l.ring.submitter().register_files_update(slot, &[fd]);
                }
                let _ = ack.send(());
            }
            Ctl::UnregisterFiles(slots, ack) => {
                for slot in slots {
                    let _ = l.ring.submitter().register_files_update(slot, &[-1]);
                }
                let _ = ack.send(());
            }
            Ctl::Publish { ver, ptr, ack } => {
                l.cfg_ptr.set(ptr);
                l.cfg_ver.set(ver);
                let _ = ack.send(());
            }
            Ctl::InstallCoreState { ptr, ack } => {
                debug_assert!(l.state_ptr.get().is_null(), "core state installed twice");
                l.state_ptr.set(ptr);
                let _ = ack.send(());
            }
            Ctl::Retire { ver, ack } => {
                // The control thread is about to drop this version; stop handing it out
                // or the maintenance tick would call the handler with a freed pointer.
                if l.cfg_ver.get() == ver {
                    l.cfg_ptr.set(std::ptr::null());
                }
                if l.guards[(ver % 4) as usize].get() == 0 {
                    let _ = ack.send(());
                } else {
                    l.pending_retire.borrow_mut().push((ver, ack));
                }
            }
            Ctl::StartQueue {
                slot,
                dev,
                cfd,
                depth,
                q_ids,
                ack,
            } => {
                self.start_queues(slot, dev, cfd, depth, &q_ids);
                let _ = ack.send(());
            }
            Ctl::StopQueue { slot, ack } => {
                let mut devs = l.devs.borrow_mut();
                let d = &mut devs[slot as usize];
                if !d.active {
                    drop(devs);
                    let _ = ack.send(());
                    return;
                }
                d.stopping = true;
                l.draining.set(true);
                d.stop_ack = Some(ack);
            }
            Ctl::Shutdown(ack) => {
                l.stop.set(true);
                let _ = ack.send(());
            }
        }
    }

    fn start_queues(&mut self, slot: u16, dev: u64, cfd: RawFd, depth: u16, q_ids: &[u16]) {
        let l = self.l;
        if let Err(e) = l
            .ring
            .submitter()
            .register_files_update(slot as u32, &[cfd])
        {
            eprintln!("racer: worker {} cannot register char device: {e}", l.core);
            return;
        }
        for (lq, &q_id) in q_ids.iter().enumerate() {
            let map = Mapping::map_read(
                unsafe { BorrowedFd::borrow_raw(cfd) },
                q_id as u64 * ublk::cmd_buf_size() as u64,
                depth as usize * size_of::<ublk::IoDesc>(),
            );
            let descs = match map {
                Ok(m) => m,
                Err(e) => {
                    eprintln!("racer: worker {} cannot map queue {q_id}: {e}", l.core);
                    return;
                }
            };
            let mut devs = l.devs.borrow_mut();
            let d = &mut devs[slot as usize];
            d.active = true;
            d.stopping = false;
            d.dev = dev;
            d.cfd = cfd;
            d.queues[lq] = Some(Queue {
                q_id,
                descs,
                armed: 0,
                inflight: 0,
                tag_state: vec![T_IDLE; depth as usize],
                tag_res: vec![0; depth as usize],
                tag_bytes: vec![0; depth as usize],
            });
        }
        for lq in 0..q_ids.len() {
            for tag in 0..depth {
                l.arm(tag_id(slot, lq, tag), ublk::IO_FETCH_REQ, 0);
            }
        }
        l.submit();
    }
}

// --- Simulation ---

/// A worker the simulator owns and steps by hand.
///
/// Same state as the real worker, minus reaping, submitting and parking. The request
/// future is boxed: `SimWorker` cannot name the anonymous `H::handle` future type.
#[cfg(feature = "sim")]
pub(crate) mod sim {
    use super::*;

    type Boxed = std::pin::Pin<Box<dyn Future<Output = Result<(), Errno>>>>;

    fn boxed<H: Handler>(h: &'static H, cfg: Cfg<H::Config>, req: Request) -> Boxed {
        Box::pin(H::handle(h, cfg, req))
    }

    pub(crate) struct SimWorker<H: Handler> {
        /// Declared before `l` so it dies first: futures drop `Cfg` guards into `Local`.
        w: Worker<'static, H, Boxed>,
        l: Box<Local>,
        /// Held so the inbox never reports itself disconnected.
        _ctl: SyncSender<Ctl>,
    }

    impl<H: Handler> SimWorker<H> {
        fn new(
            core: usize,
            fabric: Arc<Fabric>,
            handler: &'static H,
            now: Instant,
        ) -> io::Result<SimWorker<H>> {
            let l = Box::new(Local::new(core, fabric)?);
            // SAFETY: `l` is boxed so its address is stable, and `w` drops before `l`.
            let lr: &'static Local = unsafe { &*(&*l as *const Local) };
            let (tx, rx) = std::sync::mpsc::sync_channel(1);
            let w = Worker {
                l: lr,
                exec: Exec::new(
                    handler,
                    boxed::<H>,
                    (MAX_DEVICES as u32 * TAGS_PER_DEV) as usize,
                ),
                inbox: rx,
                staged: VecDeque::new(),
                last_tick: now,
            };
            Ok(SimWorker { w, l, _ctl: tx })
        }

        pub(crate) fn ring_fd(&self) -> std::os::fd::RawFd {
            self.l.ring.as_raw_fd()
        }

        /// Makes this worker the one `with_local` finds; bracket handler calls with
        /// [`leave`].
        ///
        /// [`leave`]: SimWorker::leave
        pub(crate) fn enter(&self) {
            LOCAL.with(|c| c.set(&*self.l as *const Local));
        }

        pub(crate) fn leave() {
            LOCAL.with(|c| c.set(std::ptr::null()));
        }

        /// Publishes a configuration, standing in for `Ctl::Publish`.
        pub(crate) fn publish(&self, ver: u32, ptr: *const ()) {
            self.l.cfg_ptr.set(ptr);
            self.l.cfg_ver.set(ver);
        }

        /// Installs this worker's state, standing in for `Ctl::InstallCoreState`.
        pub(crate) fn install_core_state(&self, ptr: *const ()) {
            debug_assert!(
                self.l.state_ptr.get().is_null(),
                "core state installed twice"
            );
            self.l.state_ptr.set(ptr);
        }

        /// Starts a request in slot `id`; returns a result only if it finished inline.
        pub(crate) fn start(&mut self, id: u32, req: Request) -> Option<Result<(), Errno>> {
            let cfg = self.w.l.cfg::<H::Config>();
            self.w.exec.start(id, cfg, req)
        }

        /// One turn: hops, ready tasks, maintenance. Finished requests go to `done`, not
        /// a ublk queue. Returns work done, so an idle worker is distinguishable from one
        /// between messages.
        pub(crate) fn step(
            &mut self,
            now: Instant,
            done: &mut Vec<(u32, Result<(), Errno>)>,
        ) -> usize {
            let l = self.w.l;
            let mut work = 0;
            work += l.fabric.drain(l.core);
            work += l.flush_hops();
            while let Some(id) = l.ready.pop() {
                work += 1;
                if exec::is_hop(id) {
                    poll_hop_task(exec::slot_of(id));
                } else if let Some(res) = self.w.exec.poll(id) {
                    done.push((id, res));
                }
            }
            work += self.w.maintenance(now);
            work
        }
    }

    /// One node's workers, and the hop fabric that joins them.
    pub(crate) struct SimNode<H: Handler> {
        workers: Vec<SimWorker<H>>,
        _fabric: Arc<Fabric>,
    }

    impl<H: Handler> SimNode<H> {
        pub(crate) fn new(
            cores: usize,
            handler: &'static H,
            now: Instant,
        ) -> io::Result<SimNode<H>> {
            let fabric = Arc::new(Fabric::new(cores)?);
            let mut workers = Vec::with_capacity(cores);
            for c in 0..cores {
                let w = SimWorker::new(c, fabric.clone(), handler, now)?;
                fabric.publish(c, w.ring_fd());
                workers.push(w);
            }
            Ok(SimNode {
                workers,
                _fabric: fabric,
            })
        }

        pub(crate) fn cores(&self) -> usize {
            self.workers.len()
        }

        pub(crate) fn worker(&mut self, c: usize) -> &mut SimWorker<H> {
            &mut self.workers[c]
        }

        pub(crate) fn at(&self, c: usize) -> &SimWorker<H> {
            &self.workers[c]
        }
    }

    impl<H: Handler> Drop for SimWorker<H> {
        fn drop(&mut self) {
            // The slab drops next and reaches back into `Local` to release `Cfg` guards.
            self.enter();
        }
    }
}
