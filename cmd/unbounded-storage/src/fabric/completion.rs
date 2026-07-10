// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Completion registry for libfabric one-shot operations.
//!
//! Each in-flight operation owns a `CompletionSlot`. The Rust caller
//! `Box::into_raw`s the slot and hands the resulting `*mut c_void` to
//! libfabric as the operation's `op_context`. On the progress thread,
//! `fi_cq_read` returns the same pointer back inside a
//! `fi_cq_data_entry`; we `Box::from_raw` it, release its registry
//! reservation, run its completion handler, and wake the awaiting future.
//!
//! The slot's shared state lives in an `Arc<SlotInner>` cloned into a
//! `CompletionFuture`, so completion and future-drop can race in
//! either order:
//!
//!  * If completion arrives first, the result is stored and the
//!    future picks it up on next poll.
//!  * If the future is dropped first, the Arc is released; completion
//!    still stores the result, but there is no waker to wake.
//!
//! The registry tracks live slot count purely for back-pressure
//! against `max_inflight`. The Arc identity is the slot key.

use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use super::error::{FabricError, Result};

/// Result a `CompletionSlot` carries back to its future. Per-op
/// metadata is encoded in `bytes`, `flags`, `tag` and `src_addr`
/// directly out of the `fi_cq_tagged_entry` and `fi_cq_readfrom`.
#[derive(Debug, Clone)]
pub struct CompletionInfo {
    pub flags: u64,
    pub bytes: usize,
    pub tag: u64,
    pub src_addr: u64,
    pub op_context: usize,
    /// Remote CQ immediate data (`FI_REMOTE_CQ_DATA`), valid only on
    /// completions whose `flags` carry that bit; zero otherwise.
    pub data: u64,
}

struct CompletionState {
    result: Option<Result<CompletionInfo>>,
    waker: Option<Waker>,
}

pub(crate) struct SlotInner {
    state: Mutex<CompletionState>,
}

impl SlotInner {
    fn new() -> Self {
        Self {
            state: Mutex::new(CompletionState {
                result: None,
                waker: None,
            }),
        }
    }
}

/// Per-op completion state. The Rust caller boxes one of these, hands
/// the raw box pointer to libfabric as the op_context, and clones the
/// inner `Arc` into a `CompletionFuture` to await the result.
///
/// The progress thread receives the same pointer back and calls
/// [`CompletionSlot::from_raw`] followed by [`CompletionSlot::complete`];
/// dropping the reclaimed box releases libfabric's reference.
pub struct CompletionSlot {
    inner: Arc<SlotInner>,
    /// Back-reference so the registry reservation is released before a
    /// completion handler runs, or when the box is dropped after a
    /// synchronous submission failure.
    registry: Arc<RegistryShared>,
    /// Optional handler invoked when the slot completes, before the
    /// future is woken. Used by the ping responder to re-post its
    /// recv and emit the pong without involving any future at all.
    handler: Option<Box<dyn FnOnce(&Result<CompletionInfo>) + Send>>,
    reservation_live: AtomicBool,
}

impl CompletionSlot {
    /// Hand the slot to libfabric. The returned pointer carries one
    /// owned `Box<CompletionSlot>`; pair with exactly one
    /// [`CompletionSlot::from_raw`] (typically on the progress
    /// thread) to reclaim it.
    pub fn into_raw(self: Box<Self>) -> *mut std::ffi::c_void {
        Box::into_raw(self) as *mut std::ffi::c_void
    }

    /// Inverse of [`CompletionSlot::into_raw`]. Reclaims the box.
    ///
    /// # Safety
    ///
    /// `ptr` must have been produced by [`CompletionSlot::into_raw`]
    /// and not previously reclaimed.
    pub unsafe fn from_raw(ptr: *mut std::ffi::c_void) -> Box<Self> {
        // SAFETY: caller contract.
        unsafe { Box::from_raw(ptr as *mut CompletionSlot) }
    }

    /// Release registry capacity, store a result, and wake the awaiting
    /// future. If a handler was installed via
    /// [`CompletionSlot::set_handler`] it is invoked after capacity is
    /// released and before the future is woken; the handler may not panic.
    pub fn complete(&mut self, result: Result<CompletionInfo>) {
        // Completion handlers may allocate a replacement slot, notably
        // when a receive pool rearms at full registry capacity.
        self.release_reservation();
        if let Some(h) = self.handler.take() {
            h(&result);
        }
        let waker = {
            let mut state = self.inner.state.lock().expect("completion state mutex");
            state.result = Some(result);
            state.waker.take()
        };
        if let Some(waker) = waker {
            waker.wake();
        }
    }

    /// Install a one-shot handler that fires when the slot completes.
    /// Used by self-managed slots (e.g. the ping responder) that do
    /// not have a future awaiting the outcome.
    pub fn set_handler<F>(&mut self, f: F)
    where
        F: FnOnce(&Result<CompletionInfo>) + Send + 'static,
    {
        self.handler = Some(Box::new(f));
    }

    fn release_reservation(&self) {
        if self.reservation_live.swap(false, Ordering::AcqRel) {
            self.registry.live.fetch_sub(1, Ordering::AcqRel);
        }
    }
}

impl Drop for CompletionSlot {
    fn drop(&mut self) {
        // Synchronous submission failures drop without completing. A
        // completed slot already released its reservation before running
        // its handler, so this is idempotent.
        self.release_reservation();
    }
}

/// Future a caller awaits to learn the operation outcome. Dropping
/// the future before completion does not leak: the boxed slot is
/// still reclaimed by the progress thread when libfabric raises its
/// completion, and the result simply has nobody waiting on it.
pub struct CompletionFuture {
    inner: Arc<SlotInner>,
}

impl Future for CompletionFuture {
    type Output = Result<CompletionInfo>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let mut state = self.inner.state.lock().expect("completion state mutex");
        if let Some(r) = state.result.take() {
            Poll::Ready(r)
        } else {
            state.waker = Some(cx.waker().clone());
            Poll::Pending
        }
    }
}

pub(crate) struct RegistryShared {
    cap: usize,
    live: AtomicUsize,
    peak: AtomicUsize,
}

/// Bounded back-pressure on in-flight ops. Capacity is checked at
/// allocate time; the count drops when completion begins, before its
/// handler runs, or when an unsubmitted slot is dropped after a
/// synchronous submission failure.
pub struct CompletionRegistry {
    shared: Arc<RegistryShared>,
}

impl CompletionRegistry {
    pub fn new(cap: usize) -> Arc<Self> {
        Arc::new(Self {
            shared: Arc::new(RegistryShared {
                cap: cap.max(1),
                live: AtomicUsize::new(0),
                peak: AtomicUsize::new(0),
            }),
        })
    }

    /// Allocate a fresh slot and the future bound to it. Errors with
    /// [`FabricError::BadConfig`] if the registry is full; callers
    /// surface this as back-pressure (or transport error in Phase 4).
    pub fn allocate(&self) -> Result<(Box<CompletionSlot>, CompletionFuture)> {
        // Reserve capacity first; on overflow, give it back.
        let prev = self.shared.live.fetch_add(1, Ordering::AcqRel);
        if prev >= self.shared.cap {
            self.shared.live.fetch_sub(1, Ordering::AcqRel);
            return Err(FabricError::BadConfig("completion registry full"));
        }
        let now = prev + 1;
        let mut peak = self.shared.peak.load(Ordering::Relaxed);
        while now > peak {
            match self.shared.peak.compare_exchange_weak(
                peak,
                now,
                Ordering::Relaxed,
                Ordering::Relaxed,
            ) {
                Ok(_) => break,
                Err(observed) => peak = observed,
            }
        }
        let inner = Arc::new(SlotInner::new());
        let slot = Box::new(CompletionSlot {
            inner: inner.clone(),
            registry: self.shared.clone(),
            handler: None,
            reservation_live: AtomicBool::new(true),
        });
        let fut = CompletionFuture { inner };
        Ok((slot, fut))
    }

    /// Current live slot count. Visible for tests and for metrics
    /// in later phases.
    pub fn live_count(&self) -> usize {
        self.shared.live.load(Ordering::Acquire)
    }

    pub fn available_count(&self) -> usize {
        self.shared.cap.saturating_sub(self.live_count())
    }

    /// Highest live count observed since construction. Test-only.
    pub fn peak_inflight(&self) -> usize {
        self.shared.peak.load(Ordering::Relaxed)
    }

    pub fn capacity(&self) -> usize {
        self.shared.cap
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::noop_waker;

    #[test]
    fn slot_round_trips_through_raw_context_and_resolves_future() {
        let reg = CompletionRegistry::new(4);
        let (slot, mut fut) = reg.allocate().unwrap();
        assert_eq!(reg.live_count(), 1);

        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        assert!(Pin::new(&mut fut).poll(&mut cx).is_pending());

        let raw = slot.into_raw();
        // SAFETY: raw was just produced by `into_raw`.
        let mut reclaimed = unsafe { CompletionSlot::from_raw(raw) };
        reclaimed.complete(Ok(CompletionInfo {
            flags: 0xCAFE,
            bytes: 4096,
            tag: 0,
            src_addr: 0,
            op_context: raw as usize,
            data: 0,
        }));
        drop(reclaimed);
        assert_eq!(reg.live_count(), 0);

        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Ok(info)) => {
                assert_eq!(info.flags, 0xCAFE);
                assert_eq!(info.bytes, 4096);
            }
            _ => panic!("expected ready Ok"),
        }
    }

    #[test]
    fn error_completion_propagates() {
        let reg = CompletionRegistry::new(1);
        let (slot, mut fut) = reg.allocate().unwrap();
        let raw = slot.into_raw();
        // SAFETY: raw was just produced by `into_raw`.
        let mut reclaimed = unsafe { CompletionSlot::from_raw(raw) };
        reclaimed.complete(Err(FabricError::Cq {
            prov_errno: -3,
            err: -5,
        }));
        drop(reclaimed);

        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Err(FabricError::Cq { prov_errno, err })) => {
                assert_eq!(prov_errno, -3);
                assert_eq!(err, -5);
            }
            _ => panic!("expected ready Cq error"),
        }
    }

    #[test]
    fn future_drop_without_complete_does_not_leak_slot() {
        let reg = CompletionRegistry::new(2);
        let (slot, fut) = reg.allocate().unwrap();
        assert_eq!(reg.live_count(), 1);
        // Caller drops the future before libfabric ever ran.
        drop(fut);
        // Slot still alive: the FFI side hasn't reclaimed yet.
        assert_eq!(reg.live_count(), 1);
        // Once we drop the box (which is what the progress thread
        // does on `Box::from_raw`), the live count returns to zero.
        drop(slot);
        assert_eq!(reg.live_count(), 0);
    }

    #[test]
    fn registry_capacity_enforced_and_peak_tracked() {
        let reg = CompletionRegistry::new(2);
        let (_s0, _f0) = reg.allocate().unwrap();
        let (s1, _f1) = reg.allocate().unwrap();
        // Capacity exhausted.
        assert!(matches!(
            reg.allocate(),
            Err(FabricError::BadConfig("completion registry full"))
        ));
        assert_eq!(reg.peak_inflight(), 2);
        assert_eq!(reg.capacity(), 2);
        assert_eq!(reg.available_count(), 0);
        // Releasing one slot opens room for another.
        drop(s1);
        assert_eq!(reg.available_count(), 1);
        let (_s2, _f2) = reg.allocate().unwrap();
        assert_eq!(reg.peak_inflight(), 2);
    }

    #[test]
    fn completion_handler_can_replace_slot_at_full_capacity() {
        let reg = CompletionRegistry::new(1);
        let (mut slot, _fut) = reg.allocate().unwrap();
        let replacement = Arc::new(Mutex::new(None));

        let handler_reg = Arc::clone(&reg);
        let handler_replacement = Arc::clone(&replacement);
        slot.set_handler(move |_| {
            let (slot, _fut) = handler_reg
                .allocate()
                .expect("completed slot releases capacity before handler");
            *handler_replacement.lock().unwrap() = Some(slot);
        });

        let raw = slot.into_raw();
        // SAFETY: raw was just produced by `into_raw`.
        let mut reclaimed = unsafe { CompletionSlot::from_raw(raw) };
        reclaimed.complete(Ok(CompletionInfo {
            flags: 0,
            bytes: 0,
            tag: 0,
            src_addr: 0,
            op_context: raw as usize,
            data: 0,
        }));

        assert_eq!(reg.live_count(), 1);
        drop(reclaimed);
        assert_eq!(reg.live_count(), 1);
        drop(replacement.lock().unwrap().take());
        assert_eq!(reg.live_count(), 0);
    }

    #[test]
    fn wake_before_register_still_resolves_future() {
        let reg = CompletionRegistry::new(1);
        let (slot, mut fut) = reg.allocate().unwrap();
        let raw = slot.into_raw();
        // SAFETY: raw was just produced by `into_raw`.
        let mut reclaimed = unsafe { CompletionSlot::from_raw(raw) };
        reclaimed.complete(Ok(CompletionInfo {
            flags: 0,
            bytes: 0,
            tag: 0,
            src_addr: 0,
            op_context: raw as usize,
            data: 0,
        }));
        drop(reclaimed);

        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        match Pin::new(&mut fut).poll(&mut cx) {
            Poll::Ready(Ok(_)) => {}
            _ => panic!("expected Ready(Ok)"),
        }
    }
}
