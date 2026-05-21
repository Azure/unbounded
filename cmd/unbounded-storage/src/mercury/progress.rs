// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Progress loop and completion-future primitives.
//!
//! This file is the bridge between Mercury's synchronous C callback model
//! and the rest of the crate's async code. There are five primary types:
//!
//! - [`CompletionSlot`] holds the outcome of one in-flight operation plus
//!   the [`Waker`] of the task awaiting it.
//! - [`CompletionRegistry`] owns a bounded set of `CompletionSlot`s with
//!   capacity-based backpressure, one registry per progress context.
//! - [`CompletionFuture`] is the awaitable handle a caller drops onto a
//!   slot once it has issued the corresponding Mercury call.
//! - [`Oneshot`] is a registry-less single-use slot used by callbacks
//!   that do not need backpressure (server-side bulk push / respond).
//! - [`ServerJobQueue`] is a bounded MPSC queue feeding the server's
//!   async loop from the synchronous RPC callback.
//!
//! [`progress_loop`] drives a single `hg_context_t` from a dedicated
//! thread, alternating `HG_Progress` and `HG_Trigger` until shutdown.

use std::collections::VecDeque;
use std::future::Future;
use std::os::raw::c_void;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use super::config::NicConfig;
use super::error::{HgError, Result};
use super::ffi::{self, HG_SUCCESS, hg_context_t};

// -------------------- CompletionSlot ----------------------------------

/// State of a single outstanding forward / bulk / respond callback.
///
/// The slot is shared between the FFI callback (which calls
/// [`CompletionSlot::complete`]) and the async task awaiting the
/// outcome via [`CompletionFuture`]. The `Mutex` is uncontended in the
/// common case: the producer locks once at completion time, the
/// consumer locks once per poll.
pub struct CompletionSlot {
    inner: Mutex<SlotInner>,
}

struct SlotInner {
    outcome: Option<Result<()>>,
    waker: Option<Waker>,
    /// `true` once `complete` has fired. Idempotency guard so a
    /// retransmitted callback (or a second producer racing with the
    /// first) does not overwrite the recorded outcome.
    completed: bool,
}

impl CompletionSlot {
    /// Construct an empty slot wrapped in an `Arc` for sharing with
    /// the FFI callback.
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            inner: Mutex::new(SlotInner {
                outcome: None,
                waker: None,
                completed: false,
            }),
        })
    }

    /// Publish the outcome and wake the parked task, if any.
    ///
    /// Idempotent: the second and subsequent calls are dropped on the
    /// floor. This matches Mercury's behavior where a cancelled forward
    /// can still surface a late completion callback.
    pub fn complete(&self, outcome: Result<()>) {
        let waker = {
            let mut g = self.inner.lock().expect("CompletionSlot mutex poisoned");
            if g.completed {
                return;
            }
            g.completed = true;
            g.outcome = Some(outcome);
            g.waker.take()
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    /// Take the outcome if `complete` has fired, leaving the slot
    /// drained. A second `try_take` after a successful first call
    /// returns `None`.
    pub fn try_take(&self) -> Option<Result<()>> {
        let mut g = self.inner.lock().expect("CompletionSlot mutex poisoned");
        g.outcome.take()
    }

    /// Install or refresh the parked waker.
    ///
    /// If the slot has already completed when this is called, the
    /// caller will observe the outcome on its next `try_take` without
    /// needing the waker to fire; we still install it for symmetry.
    pub fn set_waker(&self, waker: &Waker) {
        let mut g = self.inner.lock().expect("CompletionSlot mutex poisoned");
        match &g.waker {
            Some(existing) if existing.will_wake(waker) => {}
            _ => g.waker = Some(waker.clone()),
        }
    }

    /// `true` if `complete` has fired, regardless of whether the
    /// outcome has been taken yet.
    #[allow(dead_code)]
    fn is_completed(&self) -> bool {
        self.inner
            .lock()
            .expect("CompletionSlot mutex poisoned")
            .completed
    }
}

// -------------------- CompletionRegistry ------------------------------

/// Bounded slab of [`CompletionSlot`]s with allocation backpressure.
///
/// One registry lives alongside each `ProgressContext`. Callers acquire
/// a slot via [`CompletionRegistry::acquire`]; if the in-flight count
/// has reached `capacity`, the returned future parks until another
/// caller releases a slot. The capacity bookkeeping is independent of
/// the slots themselves so that a slot's outcome can be observed
/// (releasing capacity) without invalidating the slot's `Arc`.
pub struct CompletionRegistry {
    inner: Mutex<RegistryInner>,
    capacity: usize,
}

struct RegistryInner {
    in_flight: usize,
    waiters: VecDeque<Waker>,
}

impl CompletionRegistry {
    /// Construct a registry with room for `capacity` outstanding
    /// completions. `capacity` of zero means every `acquire` parks
    /// forever; callers should reject that at config time.
    pub fn new(capacity: usize) -> Arc<Self> {
        Arc::new(Self {
            inner: Mutex::new(RegistryInner {
                in_flight: 0,
                waiters: VecDeque::new(),
            }),
            capacity,
        })
    }

    /// Synchronously try to allocate a slot. Returns `None` when the
    /// registry is at capacity.
    pub fn try_alloc(&self) -> Option<Arc<CompletionSlot>> {
        let mut g = self
            .inner
            .lock()
            .expect("CompletionRegistry mutex poisoned");
        if g.in_flight >= self.capacity {
            None
        } else {
            g.in_flight += 1;
            Some(CompletionSlot::new())
        }
    }

    /// Allocate a slot, awaiting capacity if needed.
    pub fn acquire<'a>(self: &'a Arc<Self>) -> AcquireFuture<'a> {
        AcquireFuture { registry: self }
    }

    /// Release one unit of capacity and wake the oldest pending
    /// `acquire` waiter. Private; called by [`RegisteredSlot`] and
    /// [`CompletionFuture`] on drop / completion.
    fn release(&self) {
        let waker = {
            let mut g = self
                .inner
                .lock()
                .expect("CompletionRegistry mutex poisoned");
            debug_assert!(g.in_flight > 0, "release without matching acquire");
            if g.in_flight > 0 {
                g.in_flight -= 1;
            }
            g.waiters.pop_front()
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    /// Capacity bound passed to [`CompletionRegistry::new`].
    pub fn capacity(&self) -> usize {
        self.capacity
    }

    /// Current count of allocated-but-not-yet-released slots.
    pub fn in_flight(&self) -> usize {
        self.inner
            .lock()
            .expect("CompletionRegistry mutex poisoned")
            .in_flight
    }
}

/// Future returned by [`CompletionRegistry::acquire`].
///
/// Polls to a [`RegisteredSlot`] once capacity is available; parks the
/// caller's waker on the registry's FIFO when it is not.
pub struct AcquireFuture<'a> {
    registry: &'a Arc<CompletionRegistry>,
}

impl<'a> Future for AcquireFuture<'a> {
    type Output = RegisteredSlot;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let registry = self.registry;
        let mut g = registry
            .inner
            .lock()
            .expect("CompletionRegistry mutex poisoned");
        if g.in_flight < registry.capacity {
            g.in_flight += 1;
            drop(g);
            Poll::Ready(RegisteredSlot {
                slot: CompletionSlot::new(),
                registry: Arc::clone(registry),
                released: false,
            })
        } else {
            // Replace any prior waker rather than appending a duplicate
            // entry on every poll; matches the bufferpool free_list
            // pattern but keeps FIFO order across distinct callers by
            // pushing only when the queue does not already hold one
            // that will wake the same task.
            let already_parked = g.waiters.iter().any(|w| w.will_wake(cx.waker()));
            if !already_parked {
                g.waiters.push_back(cx.waker().clone());
            }
            Poll::Pending
        }
    }
}

/// An allocated [`CompletionSlot`] paired with the registry handle
/// needed to release capacity exactly once.
///
/// Capacity is released in exactly one of three places:
/// 1. The `CompletionFuture` produced by [`RegisteredSlot::into_future`]
///    resolves.
/// 2. That `CompletionFuture` is dropped before resolving.
/// 3. The `RegisteredSlot` itself is dropped without ever calling
///    `into_future`.
pub struct RegisteredSlot {
    slot: Arc<CompletionSlot>,
    registry: Arc<CompletionRegistry>,
    released: bool,
}

impl RegisteredSlot {
    /// Borrow the underlying slot. Useful for installing the slot
    /// reference on a callback context before `into_future` is called.
    pub fn slot(&self) -> &Arc<CompletionSlot> {
        &self.slot
    }

    /// Convert this allocation into a [`CompletionFuture`]. The future
    /// owns the capacity release from this point on; dropping the
    /// future without polling to completion still releases.
    pub fn into_future(mut self) -> CompletionFuture {
        self.released = true;
        CompletionFuture {
            slot: Arc::clone(&self.slot),
            registry: Some(Arc::clone(&self.registry)),
            released: false,
        }
    }

    /// Produce an opaque pointer to the slot suitable for an FFI
    /// callback `arg`. Each call clones the inner `Arc` and leaks it
    /// via `Arc::into_raw`; the receiving callback must reclaim the
    /// strong reference exactly once with [`from_callback_arg`].
    #[allow(clippy::wrong_self_convention)]
    pub fn into_callback_arg(&self) -> *mut c_void {
        let cloned = Arc::clone(&self.slot);
        Arc::into_raw(cloned) as *mut c_void
    }
}

impl Drop for RegisteredSlot {
    fn drop(&mut self) {
        if !self.released {
            self.released = true;
            self.registry.release();
        }
    }
}

// -------------------- Callback arg helpers ----------------------------

/// Recover an `Arc<CompletionSlot>` from the opaque pointer originally
/// produced by [`RegisteredSlot::into_callback_arg`] or
/// [`Oneshot::into_callback_arg`].
///
/// # Safety
/// `arg` must be a non-null pointer produced by one of the above
/// methods on a still-live slot, and must not have been reclaimed.
/// Calling `from_callback_arg` more than once for the same pointer is
/// undefined behavior; clone the recovered `Arc` if you need
/// additional references.
pub unsafe fn from_callback_arg(arg: *mut c_void) -> Arc<CompletionSlot> {
    debug_assert!(!arg.is_null(), "from_callback_arg: null arg");
    // SAFETY: per the function-level safety contract, `arg` was
    // produced by `Arc::into_raw(Arc<CompletionSlot>)` and has not yet
    // been reclaimed. `Arc::from_raw` re-establishes ownership of that
    // strong reference.
    unsafe { Arc::from_raw(arg as *const CompletionSlot) }
}

// -------------------- CompletionFuture --------------------------------

/// Future resolving to the outcome of a registered completion.
///
/// Capacity is released back to the originating registry the first
/// time the future either resolves or is dropped.
pub struct CompletionFuture {
    slot: Arc<CompletionSlot>,
    registry: Option<Arc<CompletionRegistry>>,
    released: bool,
}

impl CompletionFuture {
    fn release_capacity(&mut self) {
        if !self.released {
            self.released = true;
            if let Some(r) = self.registry.take() {
                r.release();
            }
        }
    }
}

impl Future for CompletionFuture {
    type Output = Result<()>;

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // Install/refresh the waker before checking the outcome to
        // avoid a missed wakeup if the producer fires between the
        // outcome check and the waker store.
        self.slot.set_waker(cx.waker());
        match self.slot.try_take() {
            Some(outcome) => {
                self.release_capacity();
                Poll::Ready(outcome)
            }
            None => Poll::Pending,
        }
    }
}

impl Drop for CompletionFuture {
    fn drop(&mut self) {
        self.release_capacity();
    }
}

// -------------------- Oneshot -----------------------------------------

/// Single-use callback bridge with no registry / capacity bookkeeping.
///
/// Used for FFI callbacks that do not participate in per-context
/// inflight accounting: server-side `HG_Bulk_transfer` waits and
/// server-side `HG_Respond` waits.
pub struct Oneshot {
    slot: Arc<CompletionSlot>,
}

impl Oneshot {
    /// Allocate a new oneshot. Cheap; just an `Arc<CompletionSlot>`.
    pub fn new() -> Self {
        Self {
            slot: CompletionSlot::new(),
        }
    }

    /// Pointer to hand to an FFI callback's `arg`. The callback must
    /// recover the `Arc` with [`from_callback_arg`] exactly once.
    #[allow(clippy::wrong_self_convention)]
    pub fn into_callback_arg(&self) -> *mut c_void {
        let cloned = Arc::clone(&self.slot);
        Arc::into_raw(cloned) as *mut c_void
    }

    /// Convert into the future awaiting the callback.
    pub fn into_future(self) -> OneshotFuture {
        OneshotFuture { slot: self.slot }
    }
}

impl Default for Oneshot {
    fn default() -> Self {
        Self::new()
    }
}

/// Future returned by [`Oneshot::into_future`].
pub struct OneshotFuture {
    slot: Arc<CompletionSlot>,
}

impl Future for OneshotFuture {
    type Output = Result<()>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        self.slot.set_waker(cx.waker());
        match self.slot.try_take() {
            Some(outcome) => Poll::Ready(outcome),
            None => Poll::Pending,
        }
    }
}

// -------------------- ServerJobQueue ----------------------------------

/// Bounded MPSC queue feeding the server's async loop from the
/// synchronous RPC callback.
///
/// Producers are Mercury RPC callbacks (any number, all calling
/// [`ServerJobQueue::push`] synchronously). The single consumer is the
/// server's async dispatch task, awaiting [`ServerJobQueue::next_job`].
/// Closing the queue drains pending pops with `None`.
pub struct ServerJobQueue<J: Send + 'static> {
    inner: Mutex<QueueInner<J>>,
}

struct QueueInner<J> {
    queue: VecDeque<J>,
    capacity: usize,
    closed: bool,
    waiter: Option<Waker>,
}

impl<J: Send + 'static> ServerJobQueue<J> {
    /// Construct a queue bounded at `capacity` jobs.
    pub fn new(capacity: usize) -> Arc<Self> {
        Arc::new(Self {
            inner: Mutex::new(QueueInner {
                queue: VecDeque::new(),
                capacity,
                closed: false,
                waiter: None,
            }),
        })
    }

    /// Synchronous push from an RPC callback.
    ///
    /// Returns the job back inside `PushError` on failure: `Full` when
    /// at capacity (the callback should respond with `HG_Cancel`) and
    /// `Closed` when [`ServerJobQueue::close`] has been called (the
    /// server is shutting down).
    pub fn push(&self, job: J) -> std::result::Result<(), PushError<J>> {
        let waker = {
            let mut g = self.inner.lock().expect("ServerJobQueue mutex poisoned");
            if g.closed {
                return Err(PushError::Closed(job));
            }
            if g.queue.len() >= g.capacity {
                return Err(PushError::Full(job));
            }
            g.queue.push_back(job);
            g.waiter.take()
        };
        if let Some(w) = waker {
            w.wake();
        }
        Ok(())
    }

    /// Async pop. Resolves to `None` once the queue is both closed and
    /// drained.
    pub fn next_job<'a>(self: &'a Arc<Self>) -> NextJobFuture<'a, J> {
        NextJobFuture { queue: self }
    }

    /// Mark the queue closed. Subsequent `push` calls fail with
    /// `Closed`; a pending `next_job` observing an empty queue
    /// resolves to `None`.
    pub fn close(&self) {
        let waker = {
            let mut g = self.inner.lock().expect("ServerJobQueue mutex poisoned");
            g.closed = true;
            g.waiter.take()
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    /// Number of jobs currently buffered.
    pub fn len(&self) -> usize {
        self.inner
            .lock()
            .expect("ServerJobQueue mutex poisoned")
            .queue
            .len()
    }

    /// `true` if the queue has no buffered jobs (regardless of close
    /// state).
    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// `true` if [`ServerJobQueue::close`] has been called.
    pub fn is_closed(&self) -> bool {
        self.inner
            .lock()
            .expect("ServerJobQueue mutex poisoned")
            .closed
    }
}

/// Failure mode returned from [`ServerJobQueue::push`]. The job is
/// returned so the caller can decide how to respond to Mercury (cancel
/// vs. drop).
#[derive(Debug)]
pub enum PushError<J> {
    /// Queue at capacity; callback should reject the RPC.
    Full(J),
    /// Queue closed; server is shutting down.
    Closed(J),
}

/// Future returned by [`ServerJobQueue::next_job`].
pub struct NextJobFuture<'a, J: Send + 'static> {
    queue: &'a Arc<ServerJobQueue<J>>,
}

impl<'a, J: Send + 'static> Future for NextJobFuture<'a, J> {
    type Output = Option<J>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let mut g = self
            .queue
            .inner
            .lock()
            .expect("ServerJobQueue mutex poisoned");
        if let Some(job) = g.queue.pop_front() {
            return Poll::Ready(Some(job));
        }
        if g.closed {
            return Poll::Ready(None);
        }
        // Single-consumer queue, so we keep at most one waker.
        match &g.waiter {
            Some(existing) if existing.will_wake(cx.waker()) => {}
            _ => g.waiter = Some(cx.waker().clone()),
        }
        Poll::Pending
    }
}

// -------------------- progress_loop -----------------------------------

/// Drive a single Mercury context until `shutdown` is set, then drain.
///
/// Designed to run on a dedicated thread (typically spawned via the
/// crate's threading helper). Returns `Ok(())` once shutdown drain
/// completes; never returns errors today because Mercury's
/// `HG_Progress` legitimately returns non-success rcs (timeout, no
/// pending operations, etc.) on the happy path.
pub fn progress_loop(ctx_raw: hg_context_t, shutdown: &AtomicBool, cfg: &NicConfig) -> Result<()> {
    if ctx_raw.is_null() {
        return Err(HgError::BadConfig("progress_loop: null context"));
    }

    while !shutdown.load(Ordering::Acquire) {
        // SAFETY: `ctx_raw` is a non-null Mercury context that the
        // caller guarantees is alive for the duration of this call.
        let _ = unsafe { ffi::HG_Progress(ctx_raw, cfg.progress_timeout_ms) };

        let mut actual: u32 = 0;
        // SAFETY: same as above; `&mut actual` is a valid pointer to a
        // u32 owned by this stack frame.
        let rc =
            unsafe { ffi::HG_Trigger(ctx_raw, 0, cfg.trigger_max_count, &mut actual as *mut u32) };
        if rc != HG_SUCCESS || actual == 0 {
            continue;
        }
    }

    // Final drain. Cap iterations to avoid pathological infinite loops
    // if Mercury ever fails to make progress.
    for _ in 0..1024 {
        let mut actual: u32 = 0;
        // SAFETY: same as the in-loop call above.
        let rc =
            unsafe { ffi::HG_Trigger(ctx_raw, 0, cfg.trigger_max_count, &mut actual as *mut u32) };
        if rc != HG_SUCCESS || actual == 0 {
            break;
        }
    }

    Ok(())
}

// =====================================================================
// Tests
// =====================================================================

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::AtomicUsize;

    // -- Noop waker ----------------------------------------------------

    fn noop_raw_waker() -> std::task::RawWaker {
        fn no_op(_: *const ()) {}
        fn clone(_: *const ()) -> std::task::RawWaker {
            noop_raw_waker()
        }
        let vt = &std::task::RawWakerVTable::new(clone, no_op, no_op, no_op);
        std::task::RawWaker::new(std::ptr::null(), vt)
    }

    fn noop_waker() -> Waker {
        // SAFETY: the vtable is `'static`, all functions are no-ops,
        // and the data pointer is never dereferenced.
        unsafe { Waker::from_raw(noop_raw_waker()) }
    }

    fn block_on<F: Future>(mut f: F) -> F::Output {
        // SAFETY: `f` is owned by this stack frame for the duration of
        // the call and is not moved after pinning.
        let mut f = unsafe { Pin::new_unchecked(&mut f) };
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        for _ in 0..1_000_000 {
            match f.as_mut().poll(&mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => continue,
            }
        }
        panic!("block_on: future did not complete within 1M polls");
    }

    // -- Counting waker ------------------------------------------------

    fn counting_raw_waker(counter: *const AtomicUsize) -> std::task::RawWaker {
        unsafe fn clone(data: *const ()) -> std::task::RawWaker {
            counting_raw_waker(data as *const AtomicUsize)
        }
        unsafe fn wake(data: *const ()) {
            // SAFETY: `data` was produced from a `&AtomicUsize` whose
            // lifetime outlives the waker by construction in tests.
            unsafe { (*(data as *const AtomicUsize)).fetch_add(1, Ordering::SeqCst) };
        }
        unsafe fn wake_by_ref(data: *const ()) {
            // SAFETY: same as `wake`.
            unsafe { (*(data as *const AtomicUsize)).fetch_add(1, Ordering::SeqCst) };
        }
        unsafe fn drop_(_: *const ()) {}
        let vt = &std::task::RawWakerVTable::new(clone, wake, wake_by_ref, drop_);
        std::task::RawWaker::new(counter as *const (), vt)
    }

    fn counting_waker(counter: &AtomicUsize) -> Waker {
        // SAFETY: caller keeps `counter` alive at least as long as the
        // returned waker (it's borrowed for the test's duration).
        unsafe { Waker::from_raw(counting_raw_waker(counter as *const AtomicUsize)) }
    }

    // -- CompletionSlot ------------------------------------------------

    #[test]
    fn slot_complete_then_take_round_trip() {
        let s = CompletionSlot::new();
        s.complete(Ok(()));
        let out = s.try_take().expect("outcome present");
        assert!(out.is_ok());
        assert!(s.try_take().is_none(), "second take returns None");
    }

    #[test]
    fn slot_complete_is_idempotent() {
        let s = CompletionSlot::new();
        s.complete(Ok(()));
        s.complete(Err(HgError::Closed));
        let out = s.try_take().expect("outcome present");
        assert!(out.is_ok(), "first outcome wins");
    }

    #[test]
    fn completion_future_wakes_on_complete() {
        let counter = AtomicUsize::new(0);
        let waker = counting_waker(&counter);
        let mut cx = Context::from_waker(&waker);

        let slot = CompletionSlot::new();
        let mut fut = Box::pin(CompletionFuture {
            slot: Arc::clone(&slot),
            registry: None,
            released: false,
        });

        assert!(matches!(fut.as_mut().poll(&mut cx), Poll::Pending));
        assert_eq!(counter.load(Ordering::SeqCst), 0);

        slot.complete(Ok(()));
        assert_eq!(counter.load(Ordering::SeqCst), 1, "wake fired");

        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(Ok(())) => {}
            other => panic!("expected Ready(Ok), got {other:?}"),
        }
    }

    // -- CompletionRegistry --------------------------------------------

    #[test]
    fn registry_try_alloc_respects_capacity() {
        let r = CompletionRegistry::new(2);
        let _a = r.try_alloc().expect("first alloc ok");
        let _b = r.try_alloc().expect("second alloc ok");
        assert!(r.try_alloc().is_none(), "third alloc blocked");
        assert_eq!(r.in_flight(), 2);
        assert_eq!(r.capacity(), 2);
    }

    #[test]
    fn registry_acquire_wakes_on_release() {
        let counter = AtomicUsize::new(0);
        let waker = counting_waker(&counter);
        let mut cx = Context::from_waker(&waker);

        let r = CompletionRegistry::new(1);

        // First acquire resolves immediately.
        let first = block_on(r.acquire());
        assert_eq!(r.in_flight(), 1);

        // Second acquire parks.
        let acquire2 = r.acquire();
        let mut acquire2 = Box::pin(acquire2);
        assert!(matches!(acquire2.as_mut().poll(&mut cx), Poll::Pending));
        assert_eq!(counter.load(Ordering::SeqCst), 0);

        // Drop first; capacity is released and waker fires.
        drop(first);
        assert_eq!(counter.load(Ordering::SeqCst), 1);

        match acquire2.as_mut().poll(&mut cx) {
            Poll::Ready(_slot) => {}
            Poll::Pending => panic!("acquire2 still pending after release"),
        }
    }

    #[test]
    fn registered_slot_drop_releases_capacity() {
        let r = CompletionRegistry::new(1);
        {
            let _slot = block_on(r.acquire());
            assert_eq!(r.in_flight(), 1);
        }
        assert_eq!(r.in_flight(), 0, "drop released capacity");
        // Subsequent acquire succeeds.
        let _slot = block_on(r.acquire());
        assert_eq!(r.in_flight(), 1);
    }

    #[test]
    fn callback_arg_round_trip_propagates_outcome() {
        let r = CompletionRegistry::new(1);
        let allocated = block_on(r.acquire());
        let arg = allocated.into_callback_arg();
        let fut = allocated.into_future();
        // SAFETY: `arg` was produced by `into_callback_arg` above and
        // has not been reclaimed yet.
        let recovered = unsafe { from_callback_arg(arg) };
        recovered.complete(Ok(()));
        let outcome = block_on(fut);
        assert!(outcome.is_ok());
        assert_eq!(r.in_flight(), 0, "completion released capacity");
    }

    // -- Oneshot -------------------------------------------------------

    #[test]
    fn oneshot_complete_via_callback_arg() {
        let one = Oneshot::new();
        let arg = one.into_callback_arg();
        let fut = one.into_future();
        // SAFETY: `arg` was produced just above and not yet reclaimed.
        let recovered = unsafe { from_callback_arg(arg) };
        recovered.complete(Ok(()));
        let outcome = block_on(fut);
        assert!(outcome.is_ok());
    }

    // -- ServerJobQueue ------------------------------------------------

    #[test]
    fn job_queue_push_next_round_trip() {
        let q: Arc<ServerJobQueue<u32>> = ServerJobQueue::new(4);
        q.push(7).expect("push ok");
        let got = block_on(q.next_job());
        assert_eq!(got, Some(7));
    }

    #[test]
    fn job_queue_close_drains_then_resolves_none() {
        let q: Arc<ServerJobQueue<u32>> = ServerJobQueue::new(4);
        q.push(1).unwrap();
        q.push(2).unwrap();
        q.close();
        assert_eq!(block_on(q.next_job()), Some(1));
        assert_eq!(block_on(q.next_job()), Some(2));
        assert_eq!(block_on(q.next_job()), None);
        assert!(matches!(q.push(3), Err(PushError::Closed(3))));
    }

    #[test]
    fn job_queue_full_returns_error_with_job() {
        let q: Arc<ServerJobQueue<u32>> = ServerJobQueue::new(1);
        q.push(10).unwrap();
        match q.push(20) {
            Err(PushError::Full(20)) => {}
            other => panic!("expected Full(20), got {other:?}"),
        }
    }

    #[test]
    fn job_queue_next_parks_and_wakes_on_push() {
        let counter = AtomicUsize::new(0);
        let waker = counting_waker(&counter);
        let mut cx = Context::from_waker(&waker);

        let q: Arc<ServerJobQueue<u32>> = ServerJobQueue::new(4);
        let next = q.next_job();
        let mut next = Box::pin(next);
        assert!(matches!(next.as_mut().poll(&mut cx), Poll::Pending));
        assert_eq!(counter.load(Ordering::SeqCst), 0);

        q.push(42).unwrap();
        assert_eq!(counter.load(Ordering::SeqCst), 1);

        match next.as_mut().poll(&mut cx) {
            Poll::Ready(Some(42)) => {}
            other => panic!("expected Ready(Some(42)), got {other:?}"),
        }
    }

    // -- progress_loop signature ---------------------------------------

    fn _progress_loop_takes_atomic(_: hg_context_t, _: &AtomicBool, _: &NicConfig) {
        // Type-check the signature only; never call. A real
        // `hg_context_t` is not available in unit tests.
    }
}
