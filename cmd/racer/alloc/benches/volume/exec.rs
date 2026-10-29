//! Minimal single-threaded executor over a uring-runtime reactor.
//!
//! Only woken tasks are polled. When no task was woken and the reactor
//! delivered no completion, the loop spins `spin` more turns and then blocks in
//! [`Reactor::wait`], which `ppoll`s the ring descriptor (it wakes as soon as a
//! CQE is posted) bounded by the next timer and the reactor's 10 ms cap. The
//! loop therefore does not burn CPU while it waits for the device, so thread
//! CPU time against wall time shows whether a worker is CPU bound.

use std::cell::{Cell, RefCell};
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::task::{Context, Poll, Wake, Waker};
use std::time::{Duration, Instant};

use uring_runtime::{Scope, reactor::Reactor};

/// Allocator and runtime errors kept apart.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum BenchError {
    /// Allocator error.
    Alloc(page_alloc::Error),

    /// Runtime error.
    Runtime(uring_runtime::Error),
}

impl From<page_alloc::Error> for BenchError {
    fn from(error: page_alloc::Error) -> Self {
        Self::Alloc(error)
    }
}

impl From<uring_runtime::Error> for BenchError {
    fn from(error: uring_runtime::Error) -> Self {
        Self::Runtime(error)
    }
}

impl std::fmt::Display for BenchError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}

impl std::error::Error for BenchError {}

/// Always-live scope.
#[derive(Clone)]
pub struct BenchScope;

impl Scope for BenchScope {
    type Error = BenchError;

    fn check(&self) -> Result<(), BenchError> {
        Ok(())
    }
}

/// Shared scope instance.
pub static SCOPE: BenchScope = BenchScope;

/// Reactor type used by the benchmark.
pub type BenchReactor = Reactor<BenchScope, ()>;

/// Boxed local task.
pub type Task<'a> = Pin<Box<dyn Future<Output = ()> + 'a>>;

/// Most time one `wait` may block, matching the reactor's own cap.
const MAX_WAIT: Duration = Duration::from_millis(10);

/// CQEs processed per `poll_budgeted` call.
const POLL_BUDGET: usize = 256;

/// Executor loop breakdown.
#[derive(Clone, Copy, Default)]
pub struct ExecStats {
    /// Time polling tasks.
    pub task_ns: u64,

    /// Time in `poll_budgeted` (completion processing and submission).
    pub reactor_ns: u64,

    /// Time blocked in `wait`.
    pub wait_ns: u64,

    /// Calls to `wait`.
    pub waits: u64,

    /// Task polls.
    pub task_polls: u64,

    /// Completions returned by `poll_budgeted`.
    pub completions: u64,

    /// Loop turns.
    pub turns: u64,

    /// Turns that did nothing and did not wait (spin).
    pub spins: u64,
}

impl ExecStats {
    /// Record into a report under `exec_` names.
    pub fn report(&self, report: &mut crate::stats::Report) {
        report.add("exec_task_ns", self.task_ns);
        report.add("exec_reactor_ns", self.reactor_ns);
        report.add("exec_wait_ns", self.wait_ns);
        report.add("exec_waits", self.waits);
        report.add("exec_task_polls", self.task_polls);
        report.add("exec_completions", self.completions);
        report.add("exec_turns", self.turns);
        report.add("exec_spins", self.spins);
    }
}

struct Flag {
    woken: AtomicBool,
    any: Arc<AtomicUsize>,
}

impl Wake for Flag {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        if !self.woken.swap(true, Ordering::AcqRel) {
            self.any.fetch_add(1, Ordering::AcqRel);
        }
    }
}

/// Timer wheel for one executor: sleeping tasks and their deadlines.
#[derive(Default)]
pub struct Timers {
    pending: RefCell<Vec<(Instant, Waker)>>,
}

impl Timers {
    /// Sleep until `at`.
    pub fn sleep_until(&self, at: Instant) -> Sleep<'_> {
        Sleep { timers: self, at }
    }

    /// Sleep for `duration`.
    pub fn sleep(&self, duration: Duration) -> Sleep<'_> {
        self.sleep_until(Instant::now() + duration)
    }

    /// Wake expired timers. Returns the next deadline.
    fn fire(&self, now: Instant) -> Option<Instant> {
        let mut pending = self.pending.borrow_mut();
        let mut next: Option<Instant> = None;
        pending.retain(|(at, waker)| {
            if *at <= now {
                waker.wake_by_ref();
                false
            } else {
                next = Some(next.map_or(*at, |n| n.min(*at)));
                true
            }
        });
        next
    }
}

/// Future returned by [`Timers::sleep_until`].
pub struct Sleep<'a> {
    timers: &'a Timers,
    at: Instant,
}

impl Future for Sleep<'_> {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if Instant::now() >= self.at {
            return Poll::Ready(());
        }
        self.timers
            .pending
            .borrow_mut()
            .push((self.at, cx.waker().clone()));
        Poll::Pending
    }
}

/// Run `tasks` to completion on this thread.
pub fn run<'a>(
    reactor: &BenchReactor,
    timers: &Timers,
    tasks: Vec<Task<'a>>,
    spin: u32,
    stats: &mut ExecStats,
) -> Result<(), uring_runtime::Error> {
    let any = Arc::new(AtomicUsize::new(tasks.len()));
    let flags: Vec<Arc<Flag>> = tasks
        .iter()
        .map(|_| {
            Arc::new(Flag {
                woken: AtomicBool::new(true),
                any: any.clone(),
            })
        })
        .collect();
    let wakers: Vec<Waker> = flags.iter().map(|f| Waker::from(f.clone())).collect();
    let mut tasks: Vec<Option<Task<'a>>> = tasks.into_iter().map(Some).collect();
    let mut live = tasks.len();
    let mut idle = 0u32;
    let mut now = Instant::now();
    while live > 0 {
        stats.turns += 1;
        let mut polled = 0u64;
        if any.swap(0, Ordering::AcqRel) != 0 {
            for (index, slot) in tasks.iter_mut().enumerate() {
                let Some(task) = slot else { continue };
                if !flags[index].woken.swap(false, Ordering::AcqRel) {
                    continue;
                }
                polled += 1;
                let mut cx = Context::from_waker(&wakers[index]);
                if task.as_mut().poll(&mut cx).is_ready() {
                    *slot = None;
                    live -= 1;
                }
            }
        }
        let after_tasks = Instant::now();
        stats.task_ns += (after_tasks - now).as_nanos() as u64;
        stats.task_polls += polled;
        let completed = reactor.poll_budgeted(POLL_BUDGET)?;
        let after_reactor = Instant::now();
        stats.reactor_ns += (after_reactor - after_tasks).as_nanos() as u64;
        stats.completions += completed as u64;
        now = after_reactor;
        let next = timers.fire(now);
        if polled == 0 && completed == 0 && live > 0 && any.load(Ordering::Acquire) == 0 {
            idle += 1;
            if idle > spin {
                let duration = next
                    .map_or(MAX_WAIT, |at| at.saturating_duration_since(now))
                    .min(MAX_WAIT);
                if !duration.is_zero() {
                    reactor.wait(duration)?;
                    stats.waits += 1;
                    let woke = Instant::now();
                    stats.wait_ns += (woke - now).as_nanos() as u64;
                    now = woke;
                }
            } else {
                stats.spins += 1;
            }
        } else {
            idle = 0;
        }
    }
    Ok(())
}

/// Drive one future to completion.
pub fn block_on<'a, T: 'a>(
    reactor: &BenchReactor,
    timers: &Timers,
    future: impl Future<Output = T> + 'a,
) -> T {
    let output: Rc<Cell<Option<T>>> = Rc::new(Cell::new(None));
    let slot = output.clone();
    let task: Task<'a> = Box::pin(async move {
        slot.set(Some(future.await));
    });
    let mut stats = ExecStats::default();
    run(reactor, timers, vec![task], 0, &mut stats).expect("reactor failed");
    output.take().expect("task completed")
}
