//! The worker: one io_uring per physical core, one thread, no locks on the hot path.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::future::Future;
use std::io;
use std::rc::{Rc, Weak};
use std::sync::Arc;
use std::sync::mpsc::{Receiver, Sender};
use std::task::Waker;
use std::time::{Duration, Instant};

use crate::kernel;
use crate::kernel::ring::{Cqe, Op, Sqe, Timespec};

use super::exec::{self, Exec};
use super::hop::{self, Fabric};
use super::io::OpSlab;
use super::limits::Limits;
use super::pool::{self, Pool};
use super::sys;
use super::ublk;
use super::{Errno, Handler, Request, limits, now};

const SPIN_FLOOR: Duration = Duration::from_micros(20);
const SPIN_CEILING: Duration = Duration::from_millis(1);
const TICK_INTERVAL: Duration = Duration::from_millis(1);
const PARK_TIMEOUT: Duration = TICK_INTERVAL;

const CLASS_OP: u64 = 0;
const CLASS_LINK: u64 = 1;
const CLASS_DOORBELL: u64 = 3;

const fn ud(class: u64, seq: u64, slot: u64, payload: u64) -> u64 {
    (class << 60) | ((seq & 0xFFFF) << 44) | ((slot & 0xF_FFFF) << 24) | (payload & 0xFF_FFFF)
}

pub(super) fn ud_op(idx: u32, seq: u16) -> u64 {
    ud(CLASS_OP, seq as u64, idx as u64, 0)
}
pub(super) fn ud_link(idx: u32, seq: u16) -> u64 {
    ud(CLASS_LINK, seq as u64, idx as u64, 0)
}
const UD_DOORBELL: u64 = ud(CLASS_DOORBELL, 0, 0, 0);

fn ud_class(u: u64) -> u64 {
    u >> 60
}
fn ud_gen(u: u64) -> u16 {
    ((u >> 44) & 0xFFFF) as u16
}
fn ud_slot(u: u64) -> u32 {
    ((u >> 24) & 0xF_FFFF) as u32
}

#[cfg(test)]
pub(super) fn test_ud_parts(u: u64) -> (u64, u16, u32) {
    (ud_class(u), ud_gen(u), ud_slot(u))
}

#[cfg(test)]
pub(super) fn test_park_timeout() -> Duration {
    PARK_TIMEOUT
}
/// The control thread blocks on a broadcast until every worker acks or drops its ack.
pub(super) type Ack = Sender<()>;

pub(super) enum Ctl<C> {
    /// Install `(slot, fd)` pairs into the registered file table.
    RegisterFiles(Vec<(u32, kernel::FileRef)>, Ack),

    /// Clear registered file slots.
    UnregisterFiles(Vec<u32>, Ack),

    /// Build and install this worker's value for a new configuration generation.
    Install {
        config: Arc<C>,
        ack: Ack,
    },

    #[cfg(test)]
    Request {
        id: u32,
        req: super::Request,
        done: Sender<Result<(), Errno>>,
    },

    Ublk(ublk::WorkerCtl),

    Shutdown(Ack),
}

// SAFETY: control retains an `Arc<C>` until workers release the generation. Workers only
// access `C` through shared references (`C: Sync`), and the final `C` is dropped by control.
unsafe impl<C> Send for Ctl<C> {}

/// The control thread's channel for waking a worker blocked in `io_uring_enter`.
pub(super) struct Doorbell {
    ring: kernel::Ring,
    fabric: Arc<Fabric>,
    /// Scratch for the completions a doorbell earns and never reads.
    reaped: Vec<Cqe>,
}

impl Doorbell {
    pub(super) fn new(fabric: Arc<Fabric>) -> io::Result<Doorbell> {
        Ok(Doorbell {
            ring: kernel::ring_open(64, 64, false)?,
            fabric,
            reaped: Vec::new(),
        })
    }

    pub(super) fn wake(&mut self, dst: usize) {
        if push_doorbell(&self.ring, &self.fabric, dst) {
            self.ring.submit();
            // Keep the CQ from filling; these results never matter.
            self.reaped.clear();
            self.ring.drain(&mut self.reaped);
            self.reaped.clear();
        }
    }
}

/// Posts a `MSG_RING` onto `dst`'s ring if it's asleep.
fn push_doorbell(ring: &kernel::Ring, fabric: &Fabric, dst: usize) -> bool {
    std::sync::atomic::fence(std::sync::atomic::Ordering::SeqCst);
    if !fabric.is_sleeping(dst) {
        return false;
    }
    // A ring with no descriptor cannot be posted to; waking it is the scheduler's job.
    let Some(fd) = fabric.ring_fd(dst).filter(|fd| *fd >= 0) else {
        return false;
    };
    ring.push(&Sqe::new(
        Op::MsgRingData {
            fd,
            data: UD_DOORBELL,
        },
        UD_DOORBELL,
    ))
}

// The worker context reachable from any running future.
/// A future waiting on a wall-clock instant, and the waker that will move it on.
///
/// Weak, because the future may be dropped before its deadline arrives and a deadline
/// that outlived it should find nothing rather than keep it alive.
type Deadline = (Instant, Weak<RefCell<Option<Waker>>>);

pub(super) struct Local {
    pub(super) core: usize,
    /// The sizes this worker was built at. Captured once, so the hot path never reads a
    /// thread local to learn how big its own tables are.
    pub(super) limits: &'static Limits,
    pub(super) ring: kernel::Ring,
    pub(super) fabric: Arc<Fabric>,
    pub(super) ops: OpSlab,
    pub(super) pool: Pool,
    pub(super) ready: exec::Ready,
    pub(super) hops: hop::State,
    deferred: RefCell<VecDeque<Deferred>>,
    pub(super) ublk: ublk::WorkerState,
    worker: RefCell<Option<Rc<dyn std::any::Any>>>,
    stop: Cell<bool>,
    pub(super) deadlines: RefCell<Vec<Deadline>>,
}

thread_local! {
    static LOCAL: Cell<*const Local> = const { Cell::new(std::ptr::null()) };
}

/// Binds a worker to the calling thread until it is dropped, restoring whatever was bound
/// before.
///
/// A real thread carries one worker and enters it once a turn, which costs two stores. A
/// simulated thread carries every worker on the node, and a worker that blocks inside its
/// turn gives the others a turn of their own, so what was bound has to come back.
pub(super) struct Entered {
    local: *const Local,
    pool: *const pool::Pool,
}

pub(super) fn enter(local: &Local) -> Entered {
    let previous = LOCAL.with(|c| c.replace(local));
    Entered {
        local: previous,
        pool: pool::enter(&local.pool),
    }
}

impl Drop for Entered {
    fn drop(&mut self) {
        pool::leave(self.pool);
        LOCAL.with(|c| c.set(self.local));
    }
}

pub(super) fn with<R>(f: impl FnOnce(&Local) -> R) -> R {
    let p = LOCAL.with(|c| c.get());
    assert!(!p.is_null(), "racer: not on a runtime worker thread");
    f(unsafe { &*p })
}

pub(crate) fn current_worker<H: Handler>() -> Rc<H::Worker> {
    with(|l| {
        l.worker
            .borrow()
            .as_ref()
            .expect("racer: no worker value installed")
            .clone()
            .downcast::<H::Worker>()
            .expect("racer: worker value belongs to another handler")
    })
}

enum Deferred {
    One(Sqe),
    Pair(Sqe, Sqe),
}

impl Local {
    fn new(core: usize, fabric: Arc<Fabric>) -> io::Result<Self> {
        let limits = limits();
        let ring = kernel::ring_open(limits.sq_entries, limits.sq_entries * 4, true)?;

        let pool = Pool::new(limits)?;
        // A submission names a file and a buffer by index, so the tables have to exist
        // before the first one is pushed.
        ring.register_files_sparse(limits.file_slots)?;
        ring.register_buffers_sparse(limits.total_buf_slots())?;
        ring.register_buffers_update(limits.pool_buf_base(), &pool.iovecs())?;

        Ok(Local {
            core,
            limits,
            ring,
            fabric,
            ops: OpSlab::new(limits),
            pool,
            ready: exec::Ready::new(1024),
            hops: hop::State::new(limits),
            deferred: RefCell::new(VecDeque::new()),
            ublk: ublk::WorkerState::new(limits),
            worker: RefCell::new(None),
            stop: Cell::new(false),
            deadlines: RefCell::new(Vec::new()),
        })
    }

    // -- submission ---------------------------------------------------------

    pub(super) fn push(&self, e: Sqe) {
        if !self.ring.push(&e) {
            self.deferred.borrow_mut().push_back(Deferred::One(e));
        }
    }

    /// A linked pair must land in one submission or the kernel severs the link; defer both.
    pub(super) fn push_linked(&self, a: Sqe, b: Sqe) {
        if self.ring.room() >= 2 {
            self.ring.push(&a);
            self.ring.push(&b);
        } else {
            self.deferred.borrow_mut().push_back(Deferred::Pair(a, b));
        }
    }

    fn flush_deferred(&self) -> usize {
        let mut n = 0;
        loop {
            let mut d = self.deferred.borrow_mut();
            let Some(front) = d.front() else { break };
            let need = match front {
                Deferred::One(_) => 1,
                Deferred::Pair(..) => 2,
            };
            if self.ring.room() < need {
                break;
            }
            match d.pop_front().unwrap() {
                Deferred::One(e) => {
                    self.ring.push(&e);
                }
                Deferred::Pair(a, b) => {
                    self.ring.push(&a);
                    self.ring.push(&b);
                }
            }
            n += 1;
        }
        n
    }

    pub(super) fn submit(&self) {
        // An empty submission still costs a full `io_uring_enter`; only enter with SQEs.
        if self.ring.is_empty() {
            return;
        }
        self.ring.submit();
    }

    fn reap(&self, out: &mut Vec<Cqe>) {
        if self.ring.taskrun() {
            self.ring.get_events();
        }
        self.ring.drain(out);
    }

    // -- hops ---------------------------------------------------------------

    pub(super) fn send_hop(&self, dst: usize, msg: hop::Msg) {
        if self.hops.send(&self.fabric, self.core, dst, msg)
            && push_doorbell(&self.ring, &self.fabric, dst)
        {
            self.submit();
        }
    }

    fn flush_hops(&self) -> usize {
        let mut n = 0;
        while let Some(dst) = self.hops.flush_one(&self.fabric, self.core) {
            if push_doorbell(&self.ring, &self.fabric, dst) {
                self.submit();
            }
            n += 1;
        }
        n
    }

    fn have_worker(&self) -> bool {
        self.worker.borrow().is_some()
    }
}

pub(super) struct WorkerArgs<C> {
    pub(super) core: usize,
    pub(super) cpu: usize,
    pub(super) fabric: Arc<Fabric>,
    pub(super) inbox: Receiver<Ctl<C>>,
    pub(super) ready: Ack,
}

/// Hands a panicking worker to the kernel, which decides what a lost worker costs.
struct AbortOnPanic(usize);

impl Drop for AbortOnPanic {
    fn drop(&mut self) {
        if std::thread::panicking() {
            crate::kernel::on_worker_panic(self.0);
        }
    }
}

/// The generic half of a worker: the request slab, the control inbox, and where the
/// loop had got to when it last gave up its turn.
///
/// The `Local` is not a field. A worker is stepped by whoever owns its loop, and that
/// owner is the one holding the `Local` alive, so it hands it in a turn at a time. That
/// is what lets the same `Worker` run under a thread that never returns and under a
/// scheduler that gives it one turn at a time.
struct Worker<H: Handler, F> {
    exec: Exec<H, F>,
    inbox: Receiver<Ctl<H::Config>>,
    staged: VecDeque<Ctl<H::Config>>,
    last_tick: Instant,
    worker: Option<Rc<H::Worker>>,
    cqes: Vec<Cqe>,
    clock: Instant,
    idle_since: Instant,
    spin_budget: Duration,
    turn: u32,
}

/// A worker as the kernel sees it: something that starts, takes turns, and tears down.
///
/// The ring, the buffer pool and the thread locals are all built in `start`, because they
/// belong to whichever thread or fiber will actually take the turns, and a worker that
/// failed to build takes none.
struct WorkerTask<H: Handler, F> {
    args: Option<WorkerArgs<H::Config>>,
    make: fn(Rc<H::Worker>, Request) -> F,
    built: kernel::OnThread<Built<H, F>>,
    guard: Option<AbortOnPanic>,
}

/// A worker once it has been built.
///
/// Declaration order is drop order, and it is load bearing: request futures release
/// config guards and pool buffers into the `Local` as they are dropped.
struct Built<H: Handler, F> {
    w: Option<Worker<H, F>>,
    l: Box<Local>,
}

/// Builds the worker for `args`. `F`, the anonymous `H::handle` future type, is inferred
/// here and nowhere written down, which is what keeps request futures unboxed.
pub(super) fn worker_task<H: Handler + 'static>(
    args: WorkerArgs<H::Config>,
) -> impl kernel::Task + Send {
    WorkerTask::<H, _> {
        args: Some(args),
        make: H::handle,
        built: kernel::OnThread(None),
        guard: None,
    }
}

impl<H: Handler, F: Future<Output = Result<(), Errno>>> kernel::Task for WorkerTask<H, F> {
    fn start(&mut self) {
        let args = self.args.take().expect("worker started twice");
        self.guard = Some(AbortOnPanic(args.core));
        let _ = sys::pin(args.cpu);
        let cores = args.fabric.cores();
        let l = match Local::new(args.core, args.fabric.clone()) {
            Ok(l) => Box::new(l),
            Err(e) => {
                eprintln!("racer: worker {} failed to start: {e}", args.core);
                return;
            }
        };
        args.fabric.publish(args.core, l.ring.as_raw_fd());

        let w = Worker::<H, _>::new(&l, self.make, args.inbox, now());
        if args.core == 0 {
            // The slab holds one of these per request tag on the node, so growth matters.
            eprintln!(
                "racer: {cores} workers, request future is {} bytes",
                w.exec.future_size()
            );
        }
        drop(args.ready);
        self.built = kernel::OnThread(Some(Built { w: Some(w), l }));
    }

    fn turn(&mut self) -> kernel::Turn {
        let Some(Built { w: Some(w), l }) = self.built.0.as_mut() else {
            return kernel::Turn::Done;
        };
        let _entered = enter(l);
        w.turn(l)
    }

    fn finish(&mut self) {
        if let Some(Built { w, l }) = self.built.0.as_mut() {
            // The slab must die while the worker is still bound: futures release config
            // guards and pool buffers into the `Local` as they drop, and being bound is
            // how they find it. The `Local` itself needs nothing bound to go.
            let _entered = enter(l);
            *w = None;
        }
        self.built.0 = None;
        self.guard = None;
    }
}

impl<H: Handler, F: Future<Output = Result<(), Errno>>> Worker<H, F> {
    fn new(
        l: &Local,
        make: fn(Rc<H::Worker>, Request) -> F,
        inbox: Receiver<Ctl<H::Config>>,
        now: Instant,
    ) -> Self {
        Worker {
            exec: Exec::new(make, l.limits.req_buf_slots() as usize),
            inbox,
            staged: VecDeque::new(),
            last_tick: now,
            worker: None,
            cqes: Vec::with_capacity(1024),
            clock: now,
            idle_since: now,
            spin_budget: SPIN_FLOOR,
            turn: 0,
        }
    }

    fn install(&mut self, l: &Local, config: Arc<H::Config>) {
        let worker = Rc::new(H::build_worker(
            super::CoreId::of(l.core),
            config,
            self.worker.as_deref(),
        ));
        *l.worker.borrow_mut() = Some(worker.clone());
        self.worker = Some(worker);
    }

    /// One turn of the worker's loop.
    ///
    /// Everything the loop carries between turns lives on `self`, so a turn is the unit
    /// of progress whether a thread is spinning them out or a scheduler is handing them
    /// round. A turn that finds no work blocks at most `PARK_TIMEOUT`, so a caller
    /// stepping several workers by hand still gets round to all of them, and answers
    /// `Idle` so a caller that is stepping them all can tell the run is waiting on time
    /// rather than on itself.
    fn turn(&mut self, l: &Local) -> kernel::Turn {
        let mut work = 0usize;

        let mut cqes = std::mem::take(&mut self.cqes);
        cqes.clear();
        l.reap(&mut cqes);
        work += cqes.len();
        for cqe in cqes.drain(..) {
            self.handle_cqe(l, cqe);
        }
        self.cqes = cqes;

        work += self.drain_control_before_work(l);
        work += self.drain_ready(l, |l, id, res| l.ublk.finish_request(l, id, res));

        work += l.flush_deferred();
        l.ublk.update_throttle(&l.ops);
        work += l.ublk.drain_commit_backlog(l);

        work += self.maintenance(l, self.clock);

        l.submit();

        if l.stop.get() && self.quiesced(l) {
            l.worker.borrow_mut().take();
            self.worker.take();
            return kernel::Turn::Done;
        }

        // Reading the clock is a vdso call: re-read only after work or every 64th turn.
        self.turn = self.turn.wrapping_add(1);
        if work > 0 || self.turn.is_multiple_of(64) {
            self.clock = now();
        }
        if work > 0 {
            self.idle_since = self.clock;
            return kernel::Turn::Ran;
        }
        let idle = self.clock.saturating_duration_since(self.idle_since);
        if idle < self.spin_budget {
            std::hint::spin_loop();
        } else {
            let parked = self.park(l);
            self.clock = now();
            // Steer the spin budget by what the park just revealed. Work that arrived
            // before the park had a chance to block means spinning a little longer
            // would have caught it without paying a wakeup, so reach further; a park
            // that ran its course means the budget bought nothing, so give it back.
            self.spin_budget = if parked < self.spin_budget {
                (self.spin_budget * 2).min(SPIN_CEILING)
            } else {
                (self.spin_budget / 2).max(SPIN_FLOOR)
            };
        }
        kernel::Turn::Idle
    }

    fn quiesced(&self, l: &Local) -> bool {
        self.exec.live_count() == 0
            && l.ops.inflight() == 0
            && l.ready.len() == 0
            && l.hops.is_idle()
            && l.ublk.is_idle()
            && self.staged.is_empty()
    }

    fn poll_ready(&mut self, l: &Local, id: u32) -> Option<(u32, Result<(), Errno>)> {
        if exec::is_task(id) {
            l.hops.poll_task(exec::slot_of(id));
            None
        } else {
            self.exec.poll(id).map(|res| (id, res))
        }
    }

    fn drain_ready(
        &mut self,
        l: &Local,
        mut finish: impl FnMut(&Local, u32, Result<(), Errno>),
    ) -> usize {
        let mut work = l.fabric.drain(l.core) + l.flush_hops();
        while let Some(id) = l.ready.pop() {
            work += 1;
            if let Some((id, res)) = self.poll_ready(l, id) {
                finish(l, id, res);
            }
        }
        work
    }

    /// Dekker-style park: publish `Sleeping`, re-check every inbox, then block until a
    /// completion or `PARK_TIMEOUT`. A producer that missed the change already enqueued,
    /// so the re-check sees it; one that sees `Sleeping` rings a doorbell. `ETIME` retries.
    /// Returns how long the block lasted, which steers the caller's spin budget.
    fn park(&mut self, l: &Local) -> Duration {
        let mut blocked = Duration::ZERO;
        l.fabric.set_sleeping(l.core);
        if l.fabric.pending(l.core) == 0 && l.ready.len() == 0 {
            match self.inbox.try_recv() {
                Ok(m) => self.staged.push_back(m),
                Err(_) => {
                    let ts = Timespec::from_duration(PARK_TIMEOUT);
                    let entered = now();
                    l.ring.wait(&ts);
                    blocked = now().saturating_duration_since(entered);
                }
            }
        }
        l.fabric.set_running(l.core);
        blocked
    }

    // -- completions --------------------------------------------------------

    fn handle_cqe(&mut self, l: &Local, cqe: Cqe) {
        let u = cqe.user_data;
        match ud_class(u) {
            CLASS_OP => l
                .ops
                .complete(&l.pool, ud_slot(u), ud_gen(u), cqe.result, false),
            CLASS_LINK => l
                .ops
                .complete(&l.pool, ud_slot(u), ud_gen(u), cqe.result, true),
            CLASS_DOORBELL => {}
            _ => match l.ublk.handle_cqe(l, u, cqe.result) {
                Ok(Some((id, req))) => {
                    let worker = self
                        .worker
                        .as_ref()
                        .expect("no worker value installed")
                        .clone();
                    if let Some(res) = self.exec.start(id, worker, req) {
                        l.ublk.finish_request(l, id, res);
                    }
                }
                Ok(None) => {}
                Err(()) => debug_assert!(false, "racer: unknown user_data class"),
            },
        }
    }

    // -- control ------------------------------------------------------------

    fn drain_control_before_work(&mut self, l: &Local) -> usize {
        if l.have_worker() {
            return 0;
        }
        let mut n = 0;
        while !l.have_worker() {
            let m = match self.staged.pop_front() {
                Some(m) => m,
                None => match self.inbox.try_recv() {
                    Ok(m) => m,
                    Err(_) => break,
                },
            };
            n += 1;
            self.apply_ctl(l, m);
        }
        n
    }

    fn drain_control(&mut self, l: &Local) -> usize {
        let mut n = 0;
        while let Some(m) = self.staged.pop_front() {
            n += 1;
            self.apply_ctl(l, m);
        }
        while let Ok(m) = self.inbox.try_recv() {
            n += 1;
            self.apply_ctl(l, m);
        }
        n
    }

    fn maintenance(&mut self, l: &Local, now: Instant) -> usize {
        let mut n = self.drain_control(l);
        // Deadlines: fire the ones this visit has passed and forget the abandoned ones.
        if !l.deadlines.borrow().is_empty() {
            let mut due = Vec::new();
            l.deadlines.borrow_mut().retain(|(at, weak)| {
                let Some(waker) = weak.upgrade() else {
                    return false;
                };
                if now < *at {
                    return true;
                }
                due.extend(waker.borrow_mut().take());
                false
            });
            for w in due {
                n += 1;
                w.wake();
            }
        }

        n += l.ublk.maintenance(l);

        if let Some(worker) = self
            .worker
            .as_ref()
            .filter(|_| now.saturating_duration_since(self.last_tick) >= TICK_INTERVAL)
        {
            self.last_tick = now;
            H::tick(worker.clone(), now);
        }
        n
    }

    fn apply_ctl(&mut self, l: &Local, m: Ctl<H::Config>) {
        match m {
            Ctl::RegisterFiles(items, ack) => {
                for (slot, fd) in items {
                    let _ = l.ring.register_files_update(slot, &[fd]);
                }
                let _ = ack.send(());
            }
            Ctl::UnregisterFiles(slots, ack) => {
                for slot in slots {
                    let _ = l.ring.register_files_update(slot, &[kernel::FileRef::NONE]);
                }
                let _ = ack.send(());
            }
            Ctl::Install { config, ack } => {
                self.install(l, config);
                let _ = ack.send(());
            }
            #[cfg(test)]
            Ctl::Request { id, req, done } => {
                let result = self
                    .exec
                    .start(
                        id,
                        self.worker
                            .as_ref()
                            .expect("no worker value installed")
                            .clone(),
                        req,
                    )
                    .expect("test request must complete immediately");
                let _ = done.send(result);
            }
            Ctl::Ublk(ctl) => l.ublk.apply_ctl(l, ctl),
            Ctl::Shutdown(ack) => {
                l.stop.set(true);
                let _ = ack.send(());
            }
        }
    }
}
