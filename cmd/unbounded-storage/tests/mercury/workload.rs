// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::RefCell;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::{Arc, Weak};
use std::task::{Context, RawWaker, RawWakerVTable, Waker};

use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::mercury::progress::{
    CompletionFuture, CompletionRegistry, CompletionSlot, ServerJobQueue,
};

use crate::framework::executor::{Executor, RunError, yield_n};
use crate::mercury::mocks::{
    FakeProgress, MercurySimCfg, Outcome, QueueJobSpec, SlotTag, draw_outcome, draw_submit_failure,
    job_tag, make_synthetic_job, progress_task,
};
use crate::mercury::oracle::{JobExpect, SubmissionExpect, SubmissionObserved};

// ---------------------------------------------------------------------------
// Workload model.
// ---------------------------------------------------------------------------

/// Max submissions per client. Tag namespace for client slots is
/// `1 ..= clients.len() * MAX_SUBMISSIONS_PER_CLIENT`, kept well below
/// the server-job tag base (`1_000`).
const MAX_SUBMISSIONS_PER_CLIENT: u8 = 8;

#[derive(Clone, Debug)]
pub struct Workload {
    pub registry_cap: u8,
    pub max_delivery_delay: u32,
    pub error_rate: u32,
    /// Probability in `[0, 100]` that a `submit` rejects synchronously,
    /// modelling an `HG_Forward` failure. The slot is released by the
    /// client and no callback is ever scheduled.
    pub submit_failure_rate: u32,
    pub out_of_order: bool,
    pub clients: Vec<ClientSpec>,
    pub queue_jobs: Vec<JobSpec>,
    /// Number of consumer tasks to spawn against the server queue.
    /// Production assumes exactly one consumer: `ServerJobQueue`
    /// uses a single `AtomicWaker`, so multiple awaiters racing on
    /// `next_job` would drop wakeups. The workload pins this to 1
    /// and the property suite encodes that invariant.
    pub queue_consumers: u8,
    /// Index into `queue_jobs` at or after which the producer calls
    /// `close()` (i.e. jobs at indices `>= close_after` are pushed
    /// after close and should be silently dropped). Clamped to
    /// `queue_jobs.len()` so a workload with no late pushes is
    /// representable.
    pub close_after: u8,
}

#[derive(Clone, Debug)]
pub struct ClientSpec {
    /// Yields between alloc and submit; models a client that does
    /// some prep work before handing the slot to "the FFI".
    pub pre_submit_yields: u8,
    /// Yields between submit and the first poll; lets the progress
    /// task race ahead.
    pub pre_poll_yields: u8,
    /// Cancellation strategy for the completion future.
    pub cancel: CancelMode,
    /// Number of back-to-back submissions this client issues. Each
    /// submission is a fresh `(alloc, submit, await)` round so the
    /// registry sees alloc/release/alloc cycles. Bounded to
    /// `MAX_SUBMISSIONS_PER_CLIENT` to keep the step budget sane and
    /// the per-client tag namespace small.
    pub submissions: u8,
}

/// When (if ever) the client drops its `CompletionFuture` instead of
/// awaiting it to completion.
#[derive(Clone, Copy, Debug)]
pub enum CancelMode {
    /// Await the future normally.
    Never,
    /// Drop the future before its first poll.
    BeforePoll,
    /// Poll the future once (which registers a waker), then drop it.
    /// Exercises the path where the slot has a registered waker but
    /// nobody is left to consume the wake.
    AfterFirstPoll,
}

impl CancelMode {
    fn cancels(self) -> bool {
        !matches!(self, CancelMode::Never)
    }
}

#[derive(Clone, Debug)]
pub struct JobSpec {
    /// Yields the producer emits before pushing this job.
    pub pre_push_delay: u8,
}

// ---------------------------------------------------------------------------
// Proptest strategy.
// ---------------------------------------------------------------------------

pub fn workload_strategy() -> impl Strategy<Value = Workload> {
    let registry_cap = 1u8..=16;
    let max_delivery_delay = 0u32..=12;
    let error_rate = prop_oneof![
        7 => Just(0u32),
        3 => 1u32..=80,
    ];
    let submit_failure_rate = prop_oneof![
        7 => Just(0u32),
        3 => 1u32..=80,
    ];
    let out_of_order = any::<bool>();
    let clients = vec(client_strategy(), 1..=24);
    let queue_jobs = vec(job_strategy(), 0..=24);
    let queue_consumers = 1u8..=1;
    let close_after = 0u8..=24;
    (
        registry_cap,
        max_delivery_delay,
        error_rate,
        submit_failure_rate,
        out_of_order,
        clients,
        queue_jobs,
        queue_consumers,
        close_after,
    )
        .prop_map(
            |(
                registry_cap,
                max_delivery_delay,
                error_rate,
                submit_failure_rate,
                out_of_order,
                clients,
                queue_jobs,
                queue_consumers,
                close_after,
            )| Workload {
                registry_cap,
                max_delivery_delay,
                error_rate,
                submit_failure_rate,
                out_of_order,
                clients,
                queue_jobs,
                queue_consumers,
                close_after,
            },
        )
}

fn client_strategy() -> impl Strategy<Value = ClientSpec> {
    let cancel = prop_oneof![
        7 => Just(CancelMode::Never),
        2 => Just(CancelMode::BeforePoll),
        1 => Just(CancelMode::AfterFirstPoll),
    ];
    let submissions = 1u8..=MAX_SUBMISSIONS_PER_CLIENT;
    (0u8..=8, 0u8..=8, cancel, submissions).prop_map(
        |(pre_submit_yields, pre_poll_yields, cancel, submissions)| ClientSpec {
            pre_submit_yields,
            pre_poll_yields,
            cancel,
            submissions,
        },
    )
}

fn job_strategy() -> impl Strategy<Value = JobSpec> {
    (0u8..=8).prop_map(|pre_push_delay| JobSpec { pre_push_delay })
}

// ---------------------------------------------------------------------------
// Report.
// ---------------------------------------------------------------------------

#[derive(Debug)]
pub struct RunReport {
    /// Position-aligned with `observations`. One entry per
    /// (client, submission) round actually issued, in the order each
    /// round registered its expectation with the driver.
    pub submissions: Vec<SubmissionExpect>,
    pub observations: Vec<SubmissionObserved>,
    pub progress_history: Vec<(SlotTag, Outcome)>,
    pub registry_peak_inflight: usize,
    pub registry_live_at_end: usize,
    pub registry_capacity: usize,
    /// Number of `CompletionSlot` `Arc`s that were allocated during
    /// the run and could still be upgraded from a `Weak` snapshot at
    /// the end. Computed from the mock-side weak-pointer tracker, not
    /// from the registry's own bookkeeping, so it catches references
    /// that leak through the FFI side as well as the registry side.
    pub slot_weak_upgrades_at_end: usize,
    pub jobs: Vec<JobExpect>,
    /// Tags observed by consumers, in order. Multiple consumer tasks
    /// each push to the same `Vec` (via `Rc<RefCell<_>>`); the order
    /// reflects executor scheduling.
    pub jobs_observed: Vec<SlotTag>,
    pub consumers_saw_none: usize,
    pub queue_consumers: usize,
    pub steps: u64,
}

// ---------------------------------------------------------------------------
// Driver.
// ---------------------------------------------------------------------------

pub fn run_workload(seed: u64, w: Workload) -> Result<RunReport, RunError> {
    let registry = CompletionRegistry::new(w.registry_cap.max(1) as usize);
    let progress = FakeProgress::new();
    let cfg = MercurySimCfg::new();
    cfg.max_delivery_delay.set(w.max_delivery_delay);
    cfg.error_rate.set(w.error_rate);
    cfg.out_of_order.set(w.out_of_order);
    cfg.submit_failure_rate.set(w.submit_failure_rate);

    let mut exec = Executor::new(seed);

    let submissions: Rc<RefCell<Vec<SubmissionExpect>>> = Rc::new(RefCell::new(Vec::new()));
    let observations: Rc<RefCell<Vec<SubmissionObserved>>> = Rc::new(RefCell::new(Vec::new()));
    // Tracks how many client tasks are still running. The last one
    // to finish flips `FakeProgress::done` so the progress task can
    // drain and exit. Timer-based mark_done was racy: a slow client
    // could still be in `pre_submit_yields` when the timer fired,
    // leaving its final submission with nobody to complete it.
    let outstanding_clients: Rc<std::cell::Cell<usize>> =
        Rc::new(std::cell::Cell::new(w.clients.len()));
    // Weak snapshots of every successfully-allocated `CompletionSlot`.
    // Mock-side bookkeeping: at end of run we assert none can still be
    // upgraded, which proves both the registry-side `Arc` and the
    // FFI-side reclaim have dropped their strong references.
    let slot_weaks: Rc<RefCell<Vec<Weak<CompletionSlot>>>> = Rc::new(RefCell::new(Vec::new()));

    // Spawn one task per client.
    for (cid, spec) in w.clients.iter().enumerate() {
        let registry = registry.clone();
        let progress = progress.clone();
        let cfg = cfg.clone();
        let submissions = submissions.clone();
        let observations = observations.clone();
        let outstanding_clients = outstanding_clients.clone();
        let slot_weaks = slot_weaks.clone();
        let cancel = spec.cancel;
        let pre_submit = spec.pre_submit_yields as u32;
        let pre_poll = spec.pre_poll_yields as u32;
        let rounds = spec.submissions.max(1);
        // Each (client, submission) gets a unique tag. Layout
        // packs them into a contiguous u64 range starting at 1;
        // `cid * MAX_SUBMISSIONS_PER_CLIENT + sub_idx + 1` never
        // collides with the server-job tag base (1_000).
        let tag_base = (cid as SlotTag) * MAX_SUBMISSIONS_PER_CLIENT as SlotTag + 1;

        exec.spawn(async move {
            for sub_idx in 0..rounds {
                let tag = tag_base + sub_idx as SlotTag;
                yield_n(pre_submit).await;
                let slot = match registry.alloc() {
                    Ok(s) => s,
                    Err(_) => {
                        // Record the rejection and stop this client:
                        // back-to-back retries would only churn the
                        // registry without exercising new paths.
                        let mut subs = submissions.borrow_mut();
                        let mut obs = observations.borrow_mut();
                        subs.push(SubmissionExpect {
                            tag,
                            outcome: Outcome::Ok,
                            cancelled: false,
                        });
                        obs.push(SubmissionObserved::AllocRejected);
                        break;
                    }
                };
                slot_weaks.borrow_mut().push(Arc::downgrade(&slot));
                // Model a synchronous `HG_Forward` failure: release the
                // slot back to the registry and record the rejection.
                // No callback will ever fire for this tag.
                if draw_submit_failure(&cfg) {
                    registry.release(&slot);
                    drop(slot);
                    let mut subs = submissions.borrow_mut();
                    let mut obs = observations.borrow_mut();
                    subs.push(SubmissionExpect {
                        tag,
                        outcome: Outcome::Ok,
                        cancelled: false,
                    });
                    obs.push(SubmissionObserved::SubmitRejected);
                    continue;
                }
                let outcome = draw_outcome(&cfg);
                progress.submit(tag, slot.clone(), outcome);

                // Reserve aligned slots in `submissions` /
                // `observations` *before* yielding so the index is
                // stable across await points.
                let idx = {
                    let mut subs = submissions.borrow_mut();
                    let mut obs = observations.borrow_mut();
                    subs.push(SubmissionExpect {
                        tag,
                        outcome,
                        cancelled: cancel.cancels(),
                    });
                    obs.push(SubmissionObserved::Cancelled); // placeholder, overwritten below
                    subs.len() - 1
                };

                yield_n(pre_poll).await;

                let fut = CompletionFuture {
                    slot,
                    registry: registry.clone(),
                };
                let observed = match cancel {
                    CancelMode::Never => {
                        let result = fut.await;
                        let o = match (result, outcome) {
                            (Ok(()), _) => Outcome::Ok,
                            (Err(e), Outcome::Err(c)) if e.code == c => Outcome::Err(c),
                            (Err(e), _) => Outcome::Err(e.code),
                        };
                        SubmissionObserved::Resolved(o)
                    }
                    CancelMode::BeforePoll => {
                        drop(fut);
                        SubmissionObserved::Cancelled
                    }
                    CancelMode::AfterFirstPoll => {
                        poll_once_then_drop(fut);
                        SubmissionObserved::Cancelled
                    }
                };
                observations.borrow_mut()[idx] = observed;
            }
            // Last client to finish flips the progress "done" flag
            // so the progress task can drain pending entries and
            // exit. Cancelled futures may have left no pending
            // entries; either way, after every client has stopped
            // submitting, no further `submit` calls can happen.
            let remaining = outstanding_clients.get() - 1;
            outstanding_clients.set(remaining);
            if remaining == 0 {
                progress.mark_done();
            }
        });
    }

    // Spawn the simulated progress task.
    {
        let progress = progress.clone();
        let cfg = cfg.clone();
        exec.spawn(async move {
            progress_task(progress, cfg).await;
        });
    }

    // Server-job queue producer/consumers.
    let queue: Arc<ServerJobQueue> = ServerJobQueue::new();
    let jobs_observed: Rc<RefCell<Vec<SlotTag>>> = Rc::new(RefCell::new(Vec::new()));
    let consumers_saw_none: Rc<std::cell::Cell<usize>> = Rc::new(std::cell::Cell::new(0));
    let queue_consumers = w.queue_consumers.max(1) as usize;

    let job_specs: Vec<(SlotTag, QueueJobSpec, bool)> = w
        .queue_jobs
        .iter()
        .enumerate()
        .map(|(i, j)| {
            let tag = (i as SlotTag) + 1_000;
            let before_close = (i as u8) < w.close_after;
            (
                tag,
                QueueJobSpec {
                    tag,
                    pre_push_delay: j.pre_push_delay as u32,
                },
                before_close,
            )
        })
        .collect();

    let jobs: Vec<JobExpect> = job_specs
        .iter()
        .map(|(tag, _, before_close)| JobExpect {
            tag: *tag,
            before_close: *before_close,
        })
        .collect();

    // Producer task.
    {
        let queue = queue.clone();
        let job_specs = job_specs.clone();
        let close_after = w.close_after as usize;
        exec.spawn(async move {
            for (i, (_tag, spec, _before_close)) in job_specs.iter().enumerate() {
                if i == close_after {
                    queue.close();
                }
                yield_n(spec.pre_push_delay).await;
                queue.push(make_synthetic_job(spec.tag));
            }
            // If close_after >= job_specs.len(), close still has to
            // run so consumers terminate.
            if close_after >= job_specs.len() {
                queue.close();
            }
        });
    }

    // Consumer tasks.
    for _ in 0..queue_consumers {
        let queue = queue.clone();
        let jobs_observed = jobs_observed.clone();
        let consumers_saw_none = consumers_saw_none.clone();
        exec.spawn(async move {
            loop {
                match queue.next_job().await {
                    Some(job) => {
                        jobs_observed.borrow_mut().push(job_tag(&job));
                    }
                    None => {
                        consumers_saw_none.set(consumers_saw_none.get() + 1);
                        return;
                    }
                }
            }
        });
    }

    // Total submissions actually requested across all clients; used
    // to size the step budget.
    let total_subs: u64 = w.clients.iter().map(|c| c.submissions.max(1) as u64).sum();

    // If there are no clients at all (queue-only workloads), nothing
    // will ever flip `progress.done`; do it up front so the progress
    // task can exit immediately after draining its (empty) queue.
    if w.clients.is_empty() {
        progress.mark_done();
    }

    let step_budget: u64 = 256
        + total_subs * (w.max_delivery_delay as u64 + 16) * 64
        + (w.queue_jobs.len() as u64) * 64
        + (queue_consumers as u64) * 32;
    exec.run(step_budget)?;

    let progress_history = progress.history().clone();

    let submissions = Rc::try_unwrap(submissions)
        .expect("submissions Rc unique at end of run")
        .into_inner();
    let observations = Rc::try_unwrap(observations)
        .expect("observations Rc unique at end of run")
        .into_inner();
    let jobs_observed = Rc::try_unwrap(jobs_observed)
        .expect("jobs_observed Rc unique at end of run")
        .into_inner();
    let consumers_saw_none = consumers_saw_none.get();

    let slot_weaks = Rc::try_unwrap(slot_weaks)
        .expect("slot_weaks Rc unique at end of run")
        .into_inner();
    let slot_weak_upgrades_at_end = slot_weaks.iter().filter(|w| w.upgrade().is_some()).count();

    Ok(RunReport {
        submissions,
        observations,
        progress_history,
        registry_peak_inflight: registry.peak_inflight(),
        registry_live_at_end: registry.live_count(),
        registry_capacity: registry.capacity(),
        slot_weak_upgrades_at_end,
        jobs,
        jobs_observed,
        consumers_saw_none,
        queue_consumers,
        steps: exec.last_steps(),
    })
}

/// Poll a future exactly once with a no-op waker, then drop it. Used
/// by `CancelMode::AfterFirstPoll` to exercise the path where the
/// slot has registered a waker that nobody will ever consume.
fn poll_once_then_drop<F: Future>(fut: F) {
    let mut fut = fut;
    // SAFETY: `fut` lives on the local stack and is not moved after
    // the pin is created; it is dropped at the end of this function
    // while still pinned.
    let pinned = unsafe { Pin::new_unchecked(&mut fut) };
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let _ = pinned.poll(&mut cx);
}

const NOOP_WAKER_VTABLE: RawWakerVTable = RawWakerVTable::new(
    |_| RawWaker::new(std::ptr::null(), &NOOP_WAKER_VTABLE),
    |_| {},
    |_| {},
    |_| {},
);

fn noop_waker() -> Waker {
    // SAFETY: the no-op vtable has 'static lifetime and all four
    // functions are safe to call with a null data pointer.
    unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &NOOP_WAKER_VTABLE)) }
}
