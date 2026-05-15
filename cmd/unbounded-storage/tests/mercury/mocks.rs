// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! DST-aware fakes for the Mercury cross-thread bridge.
//!
//! The bridge under test is `mercury::progress`: an
//! `Arc<CompletionSlot>` is handed to the FFI as a raw pointer, and a
//! callback fires later from the progress thread. The single-threaded
//! DST executor cannot run a second OS thread, so we model the
//! progress thread as another async task. The "FFI" surface this
//! suite cares about is just the raw-pointer round trip:
//!
//!   1. The client `into_callback_arg()`s its `Arc<CompletionSlot>`
//!      and registers the resulting pointer with `FakeProgress`.
//!   2. The progress task picks one pending entry per loop iteration
//!      (uniformly via `with_sim`), `yield_n`s a random delay drawn
//!      from `MercurySimCfg`, reclaims the slot via
//!      `from_callback_arg`, and calls `slot.complete(outcome)`.
//!   3. The shared `ServerJobQueue` is exercised the same way: a
//!      producer task pushes jobs interleaved with yields and then
//!      calls `close()`; consumer tasks pull via `next_job().await`.
//!
//! Per-area knobs (delay bound, error rate, out-of-order delivery)
//! live on `MercurySimCfg` rather than the framework's `SimState`.

use std::cell::{Cell, RefCell};
use std::ffi::c_void;
use std::rc::Rc;
use std::sync::Arc;

use rand::Rng;
use unbounded_storage::mercury::progress::{CompletionSlot, ServerJob, UnsafeSendPtr};

use crate::framework::executor::{with_sim, yield_n};

/// Per-run simulation knobs for the Mercury bridge mocks. Held
/// behind an `Rc` so both the workload driver and the progress task
/// can share a single configuration instance.
pub struct MercurySimCfg {
    /// Maximum number of `yield_once` pends the progress task emits
    /// before delivering a single pending completion. Actual count
    /// per delivery is drawn uniformly from `0..=max_delivery_delay`.
    pub max_delivery_delay: Cell<u32>,
    /// Probability in `[0, 100]` that a delivery resolves with a
    /// synthetic error. `0` is the all-success regime.
    pub error_rate: Cell<u32>,
    /// If true, the progress task picks the pending entry to deliver
    /// uniformly at random; if false it delivers FIFO. Out-of-order
    /// delivery exercises the bridge's "callback for a future the
    /// client has not yet polled" cases.
    pub out_of_order: Cell<bool>,
    /// Probability in `[0, 100]` that a `submit` call fails
    /// synchronously, mirroring an `HG_Forward` rejection. The slot
    /// is released by the client and no callback is ever scheduled,
    /// so no entry is added to `pending` or the history.
    pub submit_failure_rate: Cell<u32>,
}

impl MercurySimCfg {
    pub fn new() -> Rc<Self> {
        Rc::new(Self {
            max_delivery_delay: Cell::new(0),
            error_rate: Cell::new(0),
            out_of_order: Cell::new(false),
            submit_failure_rate: Cell::new(0),
        })
    }
}

/// Returns true with probability `MercurySimCfg::submit_failure_rate / 100`.
/// All randomness flows through `with_sim` so the run is deterministic
/// in `(seed, workload)`.
pub fn draw_submit_failure(cfg: &MercurySimCfg) -> bool {
    let rate = cfg.submit_failure_rate.get();
    rate > 0 && with_sim(|s| s.rng.gen_ratio(rate.min(100), 100))
}

/// Identity tag a client stamps onto each submission so the oracle
/// and history can correlate "submitted X" with "completed X".
pub type SlotTag = u64;

/// One outcome the progress task will deliver. `Ok(())` and `Err(code)`
/// flatten down to the same `Result<(), HgError>` the production
/// callback path would store on the slot.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Outcome {
    Ok,
    Err(i32),
}

/// One in-flight submission held by the fake progress thread until
/// it fires the callback.
pub struct Pending {
    pub tag: SlotTag,
    /// Raw pointer produced by `CompletionSlot::into_callback_arg`.
    /// `UnsafeSendPtr` is `Send + Sync` only to keep the type usable
    /// across the bridge; the executor is single-threaded so no real
    /// thread crossing happens here.
    pub arg: UnsafeSendPtr,
    pub outcome: Outcome,
}

/// Shared mock state. All mutators run on the single executor
/// thread; `RefCell` is sufficient.
pub struct FakeProgress {
    pending: RefCell<Vec<Pending>>,
    history: RefCell<Vec<(SlotTag, Outcome)>>,
    /// Set by the workload driver after every client task has
    /// finished spawning its submissions and any cancellations have
    /// been initiated. The progress task exits its loop once this is
    /// set and the pending queue is empty.
    done: Cell<bool>,
}

impl FakeProgress {
    pub fn new() -> Rc<Self> {
        Rc::new(Self {
            pending: RefCell::new(Vec::new()),
            history: RefCell::new(Vec::new()),
            done: Cell::new(false),
        })
    }

    /// Called by a client task to enqueue a submission for later
    /// delivery. Mirrors `HG_Forward`: the FFI now owns the slot
    /// reference (`arg`) until the callback fires.
    pub fn submit(&self, tag: SlotTag, slot: Arc<CompletionSlot>, outcome: Outcome) {
        let arg = UnsafeSendPtr(slot.into_callback_arg());
        self.pending
            .borrow_mut()
            .push(Pending { tag, arg, outcome });
    }

    /// Mark the workload as finished so the progress task can exit.
    /// Any remaining pending entries are still delivered before exit
    /// so cancelled futures observe their callback fire (modelling
    /// Mercury's "exactly once" callback guarantee).
    pub fn mark_done(&self) {
        self.done.set(true);
    }

    /// History accessor used by invariants.
    pub fn history(&self) -> std::cell::Ref<'_, Vec<(SlotTag, Outcome)>> {
        self.history.borrow()
    }

    pub fn pending_len(&self) -> usize {
        self.pending.borrow().len()
    }

    fn pop_next(&self, cfg: &MercurySimCfg) -> Option<Pending> {
        let mut q = self.pending.borrow_mut();
        if q.is_empty() {
            return None;
        }
        let idx = if cfg.out_of_order.get() {
            with_sim(|s| s.rng.gen_range(0..q.len()))
        } else {
            0
        };
        Some(q.swap_remove(idx))
    }

    fn record(&self, tag: SlotTag, outcome: Outcome) {
        self.history.borrow_mut().push((tag, outcome));
    }
}

/// Body of the simulated progress thread. Spawned as a task by the
/// workload driver. Loops until `FakeProgress::done` is set *and*
/// every pending submission has been delivered.
pub async fn progress_task(progress: Rc<FakeProgress>, cfg: Rc<MercurySimCfg>) {
    loop {
        let delay = if cfg.max_delivery_delay.get() == 0 {
            0
        } else {
            with_sim(|s| s.rng.gen_range(0..=cfg.max_delivery_delay.get()))
        };
        yield_n(delay).await;

        let Some(p) = progress.pop_next(&cfg) else {
            if progress.done.get() {
                return;
            }
            // Nothing to do this tick; yield so other tasks can run.
            yield_n(1).await;
            continue;
        };

        // SAFETY: `p.arg` was produced by `CompletionSlot::into_callback_arg`
        // in `FakeProgress::submit`; reclaim exactly once here, mirroring
        // the production forward-callback path.
        let slot: Arc<CompletionSlot> = unsafe { CompletionSlot::from_callback_arg(p.arg.0) };
        let result = match p.outcome {
            Outcome::Ok => Ok(()),
            Outcome::Err(code) => Err(unbounded_storage::mercury::HgError::new(code, "dst-error")),
        };
        slot.complete(result);
        progress.record(p.tag, p.outcome);
        // Drop the reclaimed Arc here; the slot may still be alive
        // via the registry plus the client's `CompletionFuture`.
    }
}

/// Decide a per-submission outcome from `MercurySimCfg::error_rate`.
pub fn draw_outcome(cfg: &MercurySimCfg) -> Outcome {
    let rate = cfg.error_rate.get();
    if rate > 0 && with_sim(|s| s.rng.gen_ratio(rate.min(100), 100)) {
        Outcome::Err(-1)
    } else {
        Outcome::Ok
    }
}

// ---------------------------------------------------------------------------
// Server-job queue producer/consumer helpers.
// ---------------------------------------------------------------------------

/// One job the producer task will push onto a `ServerJobQueue`.
/// The handle/input pointers are sentinel values: nothing in the
/// bridge dereferences them, so we can stamp identity bits into
/// `handle.0` and assert that consumers see exactly those bits.
pub struct QueueJobSpec {
    pub tag: SlotTag,
    /// Number of yields the producer emits before pushing this job.
    pub pre_push_delay: u32,
}

impl Clone for QueueJobSpec {
    fn clone(&self) -> Self {
        Self {
            tag: self.tag,
            pre_push_delay: self.pre_push_delay,
        }
    }
}

/// Construct a synthetic `ServerJob`. Pointers are tagged so the
/// consumer can read them back without touching memory.
pub fn make_synthetic_job(tag: SlotTag) -> ServerJob {
    ServerJob {
        handle: UnsafeSendPtr(tag as *mut c_void),
        input_struct: UnsafeSendPtr((!tag) as *mut c_void),
    }
}

/// Recover the tag from a `ServerJob` produced by `make_synthetic_job`.
pub fn job_tag(job: &ServerJob) -> SlotTag {
    job.handle.0 as usize as SlotTag
}
