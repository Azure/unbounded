// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Deterministic single-threaded executor for DST.
//!
//! The executor owns a list of top-level futures ("tasks"). Each task
//! gets a [`Waker`] that pushes its id onto a shared ready queue when
//! woken. On every step the executor consults the seeded PRNG to pick
//! one ready task uniformly at random and polls it once. All sources
//! of non-determinism in a DST run are funneled through the same
//! [`SimState::rng`], so a `(seed, workload)` pair fully determines
//! the schedule.

use std::cell::{Cell, RefCell};
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::task::{Context, Poll, Wake, Waker};

use rand::SeedableRng;
use rand_chacha::ChaCha8Rng;

/// Per-run mutable state owned by the framework. Lives in a
/// thread-local so async code under test can pull randomness from
/// the same PRNG that drives task scheduling.
///
/// Deliberately minimal: the framework is project-agnostic, so
/// anything beyond the seeded RNG (I/O delays, fault rates, hit
/// rates, ...) belongs in project-specific state held by the mocks
/// themselves.
pub struct SimState {
    pub rng: ChaCha8Rng,
}

impl SimState {
    pub fn new(seed: u64) -> Self {
        Self {
            rng: ChaCha8Rng::seed_from_u64(seed),
        }
    }
}

thread_local! {
    static SIM: RefCell<Option<Rc<RefCell<SimState>>>> = const { RefCell::new(None) };
    static READY: RefCell<Option<Rc<RefCell<Vec<usize>>>>> = const { RefCell::new(None) };
}

/// Run `f` against the installed sim state. Panics if no executor is
/// currently running (i.e. called outside [`Executor::run`]).
pub fn with_sim<R>(f: impl FnOnce(&mut SimState) -> R) -> R {
    let rc = SIM.with(|c| {
        c.borrow()
            .as_ref()
            .expect("DST sim not installed: call inside Executor::run")
            .clone()
    });
    let mut g = rc.borrow_mut();
    f(&mut g)
}

/// Yields once, re-waking the current task immediately. Used by the
/// mocks to model "this I/O isn't ready yet"; the re-wake keeps the
/// task in the ready set so the executor can interleave it with
/// other ready tasks on subsequent steps.
pub fn yield_once() -> YieldOnce {
    YieldOnce { yielded: false }
}

pub struct YieldOnce {
    yielded: bool,
}

impl Future for YieldOnce {
    type Output = ();
    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if self.yielded {
            Poll::Ready(())
        } else {
            self.yielded = true;
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    }
}

/// Yields `n` times. Callers typically pull `n` from
/// [`SimState::rng`] to model variable-latency operations.
pub async fn yield_n(n: u32) {
    for _ in 0..n {
        yield_once().await;
    }
}

struct TaskWaker {
    id: usize,
}

impl Wake for TaskWaker {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }
    fn wake_by_ref(self: &Arc<Self>) {
        // The thread-local is installed for the duration of
        // `Executor::run`; wakers cannot outlive that scope because
        // futures (which own the wakers) are dropped when the
        // executor returns.
        READY.with(|r| {
            if let Some(q) = r.borrow().as_ref() {
                q.borrow_mut().push(self.id);
            }
        });
    }
}

/// Failure modes the executor reports back to the caller.
#[derive(Debug)]
pub enum RunError {
    /// Ran for more than `step_budget` ticks without all tasks
    /// finishing. Surfaces both liveness bugs and accidentally
    /// unbounded workloads; the caller can grow the budget if the
    /// workload is genuinely large.
    StepBudgetExceeded,
    /// No task is ready and at least one task is still alive.
    /// Indicates a real deadlock under DST: there is no waker in
    /// flight that could rescue the system.
    Deadlock,
}

pub struct Executor {
    tasks: Vec<Option<Pin<Box<dyn Future<Output = ()>>>>>,
    ready: Rc<RefCell<Vec<usize>>>,
    sim: Rc<RefCell<SimState>>,
    /// Tracks how many steps the last `run` consumed; handy for the
    /// liveness invariant in tests.
    last_steps: Cell<u64>,
}

impl Executor {
    pub fn new(seed: u64) -> Self {
        Self {
            tasks: Vec::new(),
            ready: Rc::new(RefCell::new(Vec::new())),
            sim: Rc::new(RefCell::new(SimState::new(seed))),
            last_steps: Cell::new(0),
        }
    }

    /// Mutable access to the sim state. Useful when a project needs
    /// to re-seed or inspect the PRNG between [`Executor::run`]
    /// invocations. Kept on `Executor` (rather than reaching through
    /// [`with_sim`]) so callers don't have to install the thread-local
    /// just to touch the RNG.
    #[allow(dead_code)]
    pub fn sim_mut(&self) -> std::cell::RefMut<'_, SimState> {
        self.sim.borrow_mut()
    }

    pub fn spawn<F>(&mut self, fut: F) -> usize
    where
        F: Future<Output = ()> + 'static,
    {
        let id = self.tasks.len();
        self.tasks.push(Some(Box::pin(fut)));
        self.ready.borrow_mut().push(id);
        id
    }

    pub fn last_steps(&self) -> u64 {
        self.last_steps.get()
    }

    /// Drive all spawned tasks to completion or fail with a
    /// [`RunError`]. `step_budget` bounds the total number of
    /// `poll` calls across all tasks; pick this generously relative
    /// to the workload so genuine slowness doesn't flag as a
    /// liveness bug.
    pub fn run(&mut self, step_budget: u64) -> Result<(), RunError> {
        // Install sim + ready queue as thread-locals for the
        // duration of this call. We deliberately do this even when
        // there are no tasks so test code that only touches the
        // sim (e.g. via `with_sim`) still works.
        let prev_sim = SIM.with(|c| c.borrow_mut().replace(self.sim.clone()));
        let prev_ready = READY.with(|c| c.borrow_mut().replace(self.ready.clone()));

        let result = self.run_inner(step_budget);

        SIM.with(|c| *c.borrow_mut() = prev_sim);
        READY.with(|c| *c.borrow_mut() = prev_ready);
        result
    }

    fn run_inner(&mut self, step_budget: u64) -> Result<(), RunError> {
        let mut steps: u64 = 0;
        loop {
            let alive = self.tasks.iter().any(|t| t.is_some());
            if !alive {
                self.last_steps.set(steps);
                return Ok(());
            }
            if steps >= step_budget {
                self.last_steps.set(steps);
                return Err(RunError::StepBudgetExceeded);
            }

            // Drain duplicates: a task can be woken many times
            // before it is next polled; we only need to poll it
            // once per "ready" episode.
            let task_id = {
                let mut ready = self.ready.borrow_mut();
                // Drop ids whose task has already completed.
                ready.retain(|&id| self.tasks.get(id).is_some_and(|t| t.is_some()));
                if ready.is_empty() {
                    self.last_steps.set(steps);
                    return Err(RunError::Deadlock);
                }
                let i = with_sim(|s| {
                    use rand::Rng;
                    s.rng.gen_range(0..ready.len())
                });
                let id = ready.swap_remove(i);
                // Remove any further duplicates of this id so we
                // don't re-poll it again immediately.
                ready.retain(|&x| x != id);
                id
            };

            let waker: Waker = Arc::new(TaskWaker { id: task_id }).into();
            let mut cx = Context::from_waker(&waker);
            let done = {
                let slot = self
                    .tasks
                    .get_mut(task_id)
                    .and_then(|t| t.as_mut())
                    .expect("task id was filtered to live tasks above");
                matches!(slot.as_mut().poll(&mut cx), Poll::Ready(()))
            };
            if done {
                self.tasks[task_id] = None;
            }

            steps += 1;
        }
    }
}
