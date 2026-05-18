// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Cross-thread bridges between Mercury's progress thread and the
//! single-threaded async executor that owns the bufferpool.
//!
//! Two flows cross the thread boundary:
//!
//!  1. *Completion bridge* (`CompletionRegistry`): client-side
//!     `bulk_get` futures wait for forward-RPC callbacks fired by
//!     `HG_Trigger` on the progress thread. The future allocates a
//!     `CompletionSlot`, hands its raw pointer to `HG_Forward` as
//!     the user arg, and parks via an `AtomicWaker`. The progress
//!     thread writes the result into the slot and wakes the waker.
//!
//!  2. *Server-job bridge* (`ServerJobQueue`): the registered RPC
//!     callback runs on the progress thread but the application's
//!     bulk source lives on the executor thread. The callback
//!     pushes a job onto the queue and the executor pulls jobs out
//!     of it via the future returned by `next_job`.
//!
//! Both bridges are pure synchronization primitives - no Mercury
//! calls happen here; that keeps the unsafe surface minimal and
//! testable in isolation.

use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicU8, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use crate::mercury::error::{HgError, Result};

const WAKER_NONE: u8 = 0;
const WAKER_REGISTERED: u8 = 1;
const WAKER_WOKEN: u8 = 3;

/// Minimal `AtomicWaker`: register-store-wake state machine,
/// avoiding a dependency on `futures-util`. Behaves like
/// `futures_util::task::AtomicWaker` for our single-producer
/// single-consumer use: the future registers on poll; the
/// completer wakes exactly once.
struct AtomicWaker {
    state: AtomicU8,
    waker: Mutex<Option<Waker>>,
}

impl AtomicWaker {
    fn new() -> Self {
        Self {
            state: AtomicU8::new(WAKER_NONE),
            waker: Mutex::new(None),
        }
    }

    /// Register a fresh waker for later wakeup. Idempotent; safe to
    /// call from successive polls.
    fn register(&self, w: &Waker) {
        // Fast path: if already woken, no need to store the waker.
        if self.state.load(Ordering::Acquire) == WAKER_WOKEN {
            w.wake_by_ref();
            return;
        }
        if let Ok(mut slot) = self.waker.lock() {
            *slot = Some(w.clone());
        }
        // Publish that a waker is now installed. If a wake raced us
        // and saw WAKER_NONE, the state will already be WAKER_WOKEN,
        // and the next caller path handles it.
        let prev = self.state.swap(WAKER_REGISTERED, Ordering::AcqRel);
        if prev == WAKER_WOKEN {
            if let Ok(mut slot) = self.waker.lock() {
                if let Some(w) = slot.take() {
                    w.wake();
                }
            }
        }
    }

    /// Wake the registered waker if any. Idempotent.
    fn wake(&self) {
        let prev = self.state.swap(WAKER_WOKEN, Ordering::AcqRel);
        if prev == WAKER_REGISTERED {
            if let Ok(mut slot) = self.waker.lock() {
                if let Some(w) = slot.take() {
                    w.wake();
                }
            }
        }
    }
}

/// State for one in-flight `bulk_get` future. `result` is `None`
/// until the progress thread fills it in. `cancelled` short-circuits
/// the future drop path so a callback that races a drop simply
/// stores its result and finds nobody waiting.
pub struct CompletionSlot {
    pub(crate) result: Mutex<Option<Result<()>>>,
    pub(crate) cancelled: AtomicBool,
    waker: AtomicWaker,
}

impl CompletionSlot {
    fn new() -> Self {
        Self {
            result: Mutex::new(None),
            cancelled: AtomicBool::new(false),
            waker: AtomicWaker::new(),
        }
    }

    /// Called from the progress thread when a forward callback
    /// fires. The slot retains the result regardless of cancellation
    /// because the FFI guarantees the callback runs exactly once.
    pub fn complete(&self, result: Result<()>) {
        if let Ok(mut slot) = self.result.lock() {
            *slot = Some(result);
        }
        self.waker.wake();
    }

    /// Returns whether the owning `CompletionFuture` has been dropped.
    /// Visible to tests; production code does not need it because the
    /// callback always runs to completion regardless.
    pub fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::Acquire)
    }

    /// Convert an owned `Arc<Self>` into a `*mut c_void` suitable as the
    /// user-arg for an FFI callback. The returned pointer carries one
    /// strong reference that must later be reclaimed via
    /// [`from_callback_arg`].
    pub fn into_callback_arg(self: Arc<Self>) -> *mut std::ffi::c_void {
        Arc::into_raw(self) as *mut std::ffi::c_void
    }

    /// Reclaim the strong reference produced by [`into_callback_arg`].
    ///
    /// # Safety
    ///
    /// `arg` must be a pointer produced by [`into_callback_arg`], used
    /// to reclaim that reference exactly once.
    pub unsafe fn from_callback_arg(arg: *mut std::ffi::c_void) -> Arc<Self> {
        unsafe { Arc::from_raw(arg as *const CompletionSlot) }
    }
}

/// Bounded registry of in-flight completion slots. Slot identity is
/// the `Arc<CompletionSlot>` itself; we don't hand opaque integer
/// ids to C because the FFI accepts a `void *` user arg directly,
/// which is much harder to misuse.
///
/// The "registry" exists to enforce `max_inflight` and to keep
/// strong references alive while the FFI holds the raw pointer.
pub struct CompletionRegistry {
    cap: usize,
    live: Mutex<Vec<Arc<CompletionSlot>>>,
    /// Peak observed `live.len()` across the lifetime of this
    /// registry. Used by DST tests to assert `max_inflight` was
    /// honored; production code does not read this.
    peak: AtomicUsize,
}

impl CompletionRegistry {
    pub fn new(cap: usize) -> Arc<Self> {
        Arc::new(Self {
            cap: cap.max(1),
            live: Mutex::new(Vec::with_capacity(cap.min(1024))),
            peak: AtomicUsize::new(0),
        })
    }

    /// Allocate a new slot. Fails with `HgError` if the registry is
    /// full; this surfaces as `bufferpool::Error::Transport`.
    pub fn alloc(&self) -> Result<Arc<CompletionSlot>> {
        let mut live = self.live.lock().expect("completion registry mutex");
        if live.len() >= self.cap {
            return Err(HgError::new(0, "completion registry full"));
        }
        let slot = Arc::new(CompletionSlot::new());
        live.push(slot.clone());
        let now = live.len();
        // Relaxed: we only need a monotonic upper bound for the
        // peak; readers in tests fence via their own ordering.
        let mut prev = self.peak.load(Ordering::Relaxed);
        while now > prev {
            match self
                .peak
                .compare_exchange_weak(prev, now, Ordering::Relaxed, Ordering::Relaxed)
            {
                Ok(_) => break,
                Err(observed) => prev = observed,
            }
        }
        Ok(slot)
    }

    /// Release a slot once the caller is done with it. The shared
    /// `Arc<CompletionSlot>` only goes away when *both* the
    /// registry-side and FFI-side references drop, so a late
    /// callback never dereferences freed memory.
    pub fn release(&self, slot: &Arc<CompletionSlot>) {
        if let Ok(mut live) = self.live.lock() {
            live.retain(|s| !Arc::ptr_eq(s, slot));
        }
    }

    /// Number of slots currently held by the registry. Visible to
    /// tests for the "no leak" invariant; production code does not
    /// read this.
    pub fn live_count(&self) -> usize {
        self.live.lock().map(|g| g.len()).unwrap_or(0)
    }

    /// Highest `live_count` observed since construction. Test-only.
    pub fn peak_inflight(&self) -> usize {
        self.peak.load(Ordering::Relaxed)
    }

    /// Configured upper bound. Test-only accessor.
    pub fn capacity(&self) -> usize {
        self.cap
    }
}

/// One-shot completion bundle. Bundles a fresh single-slot registry,
/// the future the caller awaits, and the raw `*mut c_void` that the
/// caller hands to an FFI submission as its user arg. If the
/// submission fails synchronously, the caller must invoke
/// [`Oneshot::reclaim`] on the arg so the leaked strong reference
/// drops instead of leaking forever.
pub(crate) struct Oneshot {
    pub(crate) future: CompletionFuture,
    pub(crate) arg: *mut std::ffi::c_void,
}

impl Oneshot {
    /// Build a fresh oneshot bundle. The slot's strong-ref count is
    /// two: one held by the future, one leaked into `arg`.
    pub(crate) fn new() -> Self {
        let registry = CompletionRegistry::new(1);
        let slot = registry.alloc().expect("oneshot registry has capacity 1");
        let arg = slot.clone().into_callback_arg();
        Self {
            future: CompletionFuture { slot, registry },
            arg,
        }
    }

    /// Reclaim the strong reference embedded in `arg` after an FFI
    /// submission failed and its callback will not fire.
    ///
    /// # Safety
    ///
    /// `arg` must be the pointer returned by [`Oneshot::new`] from
    /// the same bundle, and must not have been delivered to a
    /// callback.
    pub(crate) unsafe fn reclaim(arg: *mut std::ffi::c_void) {
        // SAFETY: `arg` came from `CompletionSlot::into_callback_arg`
        // in `Oneshot::new`.
        unsafe {
            let _ = CompletionSlot::from_callback_arg(arg);
        }
    }
}

/// Body shared by every one-shot FFI completion callback in the
/// crate. Reclaims the slot from `arg`, resolves it with `Ok(())` on
/// `HG_SUCCESS` or `Err(HgError::new(ret, ctx))` otherwise, and
/// returns `HG_SUCCESS` so Mercury treats the callback as handled.
///
/// # Safety
///
/// `arg` must be a pointer previously produced by
/// [`CompletionSlot::into_callback_arg`] (typically via
/// [`Oneshot::new`]) and not yet reclaimed.
pub(crate) unsafe fn complete_oneshot(
    ret: crate::mercury::ffi::hg_return_t,
    arg: *mut std::ffi::c_void,
    ctx: &'static str,
) -> crate::mercury::ffi::hg_return_t {
    // SAFETY: caller contract.
    let slot = unsafe { CompletionSlot::from_callback_arg(arg) };
    let outcome = if ret == crate::mercury::ffi::HG_SUCCESS {
        Ok(())
    } else {
        Err(HgError::new(ret as i32, ctx))
    };
    slot.complete(outcome);
    crate::mercury::ffi::HG_SUCCESS
}

/// Future returned by the transport to its caller. Drops are safe:
/// the `CompletionFuture` releases its slot through `release_on_drop`,
/// and the FFI-side `Arc` (handed in as the forward user arg) is
/// dropped from the C callback path regardless of caller liveness.
pub struct CompletionFuture {
    pub slot: Arc<CompletionSlot>,
    pub registry: Arc<CompletionRegistry>,
}

impl Future for CompletionFuture {
    type Output = Result<()>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // Register first so a wake that arrives between the check
        // and the park stores into a queued waker, not the void.
        self.slot.waker.register(cx.waker());
        let mut result = self.slot.result.lock().expect("completion slot mutex");
        if let Some(r) = result.take() {
            Poll::Ready(r)
        } else {
            Poll::Pending
        }
    }
}

impl Drop for CompletionFuture {
    fn drop(&mut self) {
        self.slot.cancelled.store(true, Ordering::Release);
        self.registry.release(&self.slot);
    }
}

// ---------------------------------------------------------------------------
// Server-job bridge.
// ---------------------------------------------------------------------------

/// One incoming RPC waiting for the executor to handle it. The
/// `hg_handle` and the decoded input are wrapped in `Send` newtypes
/// (`UnsafeSendPtr`) so the queue itself is `Send`; the executor
/// thread is responsible for honoring Mercury's threading rules
/// when it touches them (in practice we only call `HG_Bulk_transfer`
/// and `HG_Respond` from the progress thread, never the executor).
pub struct ServerJob {
    pub handle: UnsafeSendPtr,
    pub input_struct: UnsafeSendPtr,
}

/// Newtype that promises the embedder upholds Mercury's
/// thread-safety contract for the wrapped pointer. We only use it to
/// shuttle pointers across the bridge; no read or write happens
/// inside the type.
#[derive(Copy, Clone)]
pub struct UnsafeSendPtr(pub *mut std::ffi::c_void);
unsafe impl Send for UnsafeSendPtr {}
unsafe impl Sync for UnsafeSendPtr {}

/// MPSC queue feeding the executor-side server task. The progress
/// thread pushes; the executor polls `next_job`.
pub struct ServerJobQueue {
    inner: Mutex<ServerJobInner>,
    waker: AtomicWaker,
}

struct ServerJobInner {
    jobs: VecDeque<ServerJob>,
    closed: bool,
}

impl ServerJobQueue {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            inner: Mutex::new(ServerJobInner {
                jobs: VecDeque::new(),
                closed: false,
            }),
            waker: AtomicWaker::new(),
        })
    }

    /// Called from the Mercury RPC callback on the progress thread.
    #[doc(hidden)]
    pub fn push(&self, job: ServerJob) {
        if let Ok(mut inner) = self.inner.lock() {
            if !inner.closed {
                inner.jobs.push_back(job);
            }
        }
        self.waker.wake();
    }

    /// Called by `Class::shutdown` so pending `next_job` futures
    /// resolve to `None` and the executor-side server task exits.
    pub fn close(&self) {
        if let Ok(mut inner) = self.inner.lock() {
            inner.closed = true;
        }
        self.waker.wake();
    }

    /// Async pop, used by the executor-side server task to await
    /// the next RPC. Returns `None` once the queue is closed and
    /// drained.
    pub fn next_job(self: &Arc<Self>) -> NextJob {
        NextJob {
            queue: self.clone(),
        }
    }
}

pub struct NextJob {
    queue: Arc<ServerJobQueue>,
}

impl Future for NextJob {
    type Output = Option<ServerJob>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        self.queue.waker.register(cx.waker());
        let mut inner = self.queue.inner.lock().expect("server queue mutex");
        if let Some(job) = inner.jobs.pop_front() {
            Poll::Ready(Some(job))
        } else if inner.closed {
            Poll::Ready(None)
        } else {
            Poll::Pending
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::task::{RawWaker, RawWakerVTable, Waker};

    fn noop_waker() -> Waker {
        fn no(_: *const ()) {}
        fn clone(_: *const ()) -> RawWaker {
            RawWaker::new(std::ptr::null(), &VT)
        }
        static VT: RawWakerVTable = RawWakerVTable::new(clone, no, no, no);
        // SAFETY: the vtable's functions never dereference the data
        // pointer, so a null data pointer is sound for this waker.
        unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VT)) }
    }

    #[test]
    fn completion_completes_and_wakes() {
        let reg = CompletionRegistry::new(4);
        let slot = reg.alloc().unwrap();
        let mut fut = CompletionFuture {
            slot: slot.clone(),
            registry: reg.clone(),
        };
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        assert!(Pin::new(&mut fut).poll(&mut cx).is_pending());

        slot.complete(Ok(()));
        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Ok(())) => {}
            other => panic!(
                "expected ready Ok, got {:?}",
                matches!(other, Poll::Ready(_))
            ),
        }
    }

    #[test]
    fn registry_full_returns_error() {
        let reg = CompletionRegistry::new(2);
        let a = reg.alloc().unwrap();
        let _b = reg.alloc().unwrap();
        assert!(reg.alloc().is_err());
        // Releasing through the future-drop path frees a slot.
        let fut = CompletionFuture {
            slot: a.clone(),
            registry: reg.clone(),
        };
        drop(fut);
        assert!(reg.alloc().is_ok());
    }

    #[test]
    fn server_queue_round_trips() {
        let q = ServerJobQueue::new();
        let q2 = q.clone();
        let t = std::thread::spawn(move || {
            q2.push(ServerJob {
                handle: UnsafeSendPtr(0x1 as *mut _),
                input_struct: UnsafeSendPtr(0x2 as *mut _),
            });
        });
        t.join().unwrap();

        let mut fut = q.next_job();
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Some(job)) => {
                assert_eq!(job.handle.0 as usize, 0x1);
                assert_eq!(job.input_struct.0 as usize, 0x2);
            }
            _ => panic!("expected job"),
        }

        q.close();
        let mut fut = q.next_job();
        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(None) => {}
            _ => panic!("expected None after close"),
        }
    }

    #[test]
    fn completion_error_propagates() {
        let reg = CompletionRegistry::new(1);
        let slot = reg.alloc().unwrap();
        let mut fut = CompletionFuture {
            slot: slot.clone(),
            registry: reg.clone(),
        };
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        assert!(Pin::new(&mut fut).poll(&mut cx).is_pending());

        slot.complete(Err(HgError::new(-3, "boom")));
        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Err(e)) => {
                assert_eq!(e.code, -3);
                assert_eq!(e.ctx, "boom");
            }
            _ => panic!("expected ready err"),
        }
    }

    #[test]
    fn registry_peak_tracks_highest_live() {
        let reg = CompletionRegistry::new(4);
        let _a = reg.alloc().unwrap();
        let _b = reg.alloc().unwrap();
        let c = reg.alloc().unwrap();
        assert_eq!(reg.peak_inflight(), 3);
        reg.release(&c);
        drop(c);
        // Releasing does not lower the peak.
        let _d = reg.alloc().unwrap();
        assert_eq!(reg.peak_inflight(), 3);
        assert_eq!(reg.capacity(), 4);
    }

    #[test]
    fn registry_release_via_future_drop_lowers_live_count() {
        let reg = CompletionRegistry::new(2);
        let a = reg.alloc().unwrap();
        assert_eq!(reg.live_count(), 1);
        let fut = CompletionFuture {
            slot: a.clone(),
            registry: reg.clone(),
        };
        drop(fut);
        assert_eq!(reg.live_count(), 0);
        assert!(a.is_cancelled());
    }

    #[test]
    fn callback_arg_round_trip_preserves_strong_count() {
        let reg = CompletionRegistry::new(1);
        let slot = reg.alloc().unwrap();
        // alloc returns one ref; registry retains another.
        assert_eq!(Arc::strong_count(&slot), 2);
        let arg = slot.clone().into_callback_arg();
        assert_eq!(Arc::strong_count(&slot), 3);
        // SAFETY: `arg` was just produced by `into_callback_arg`.
        let reclaimed = unsafe { CompletionSlot::from_callback_arg(arg) };
        assert!(Arc::ptr_eq(&reclaimed, &slot));
        assert_eq!(Arc::strong_count(&slot), 3); // reclaim consumed the leaked ref
        drop(reclaimed);
        assert_eq!(Arc::strong_count(&slot), 2);
    }

    #[test]
    fn oneshot_reclaim_releases_strong_ref() {
        let oneshot = Oneshot::new();
        // future holds one slot ref; arg holds another.
        let slot_weak = Arc::downgrade(&oneshot.future.slot);
        let arg = oneshot.arg;
        // Drop future first so only the leaked Arc keeps the slot alive.
        drop(oneshot.future);
        assert!(slot_weak.strong_count() >= 1);
        // SAFETY: `arg` came from `Oneshot::new` and has not been delivered.
        unsafe {
            Oneshot::reclaim(arg);
        }
        assert_eq!(slot_weak.strong_count(), 0);
    }

    #[test]
    fn oneshot_future_completes_via_complete_oneshot() {
        let oneshot = Oneshot::new();
        let arg = oneshot.arg;
        let mut fut = oneshot.future;
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        assert!(Pin::new(&mut fut).poll(&mut cx).is_pending());

        // SAFETY: `arg` came from `Oneshot::new`.
        let ret = unsafe { complete_oneshot(crate::mercury::ffi::HG_SUCCESS, arg, "test-ctx") };
        assert_eq!(ret, crate::mercury::ffi::HG_SUCCESS);

        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Ok(())) => {}
            _ => panic!("expected Ready(Ok)"),
        }
    }

    #[test]
    fn complete_oneshot_propagates_error_with_ctx() {
        let oneshot = Oneshot::new();
        let arg = oneshot.arg;
        let mut fut = oneshot.future;
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        assert!(Pin::new(&mut fut).poll(&mut cx).is_pending());

        // SAFETY: `arg` came from `Oneshot::new`.
        let _ = unsafe { complete_oneshot(-9, arg, "site") };

        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Err(e)) => {
                assert_eq!(e.code, -9);
                assert_eq!(e.ctx, "site");
            }
            _ => panic!("expected Ready(Err)"),
        }
    }

    #[test]
    fn waker_wake_before_register_still_wakes() {
        // Models the race the AtomicWaker is meant to handle.
        let reg = CompletionRegistry::new(1);
        let slot = reg.alloc().unwrap();
        // Complete *before* anyone polls / registers a waker.
        slot.complete(Ok(()));

        let mut fut = CompletionFuture {
            slot,
            registry: reg.clone(),
        };
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Ok(())) => {}
            _ => panic!("expected Ready(Ok) on first poll"),
        }
    }

    #[test]
    fn server_queue_post_close_pushes_are_dropped() {
        let q = ServerJobQueue::new();
        q.push(ServerJob {
            handle: UnsafeSendPtr(0xAA as *mut _),
            input_struct: UnsafeSendPtr(0xBB as *mut _),
        });
        q.close();
        // Post-close push silently dropped.
        q.push(ServerJob {
            handle: UnsafeSendPtr(0xCC as *mut _),
            input_struct: UnsafeSendPtr(0xDD as *mut _),
        });

        let mut fut = q.next_job();
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Some(job)) => {
                assert_eq!(job.handle.0 as usize, 0xAA);
            }
            _ => panic!("expected pre-close job"),
        }
        let mut fut = q.next_job();
        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(None) => {}
            _ => panic!("expected None: post-close push must be dropped"),
        }
    }
}
