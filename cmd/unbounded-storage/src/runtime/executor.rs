// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! The crate's shared, runtime-agnostic `block_on` executors.
//!
//! `unbounded-storage` deliberately pulls in no async runtime (no
//! tokio, no async-std): futures are driven by hand wherever a
//! synchronous thread needs to wait on one. This module owns the two
//! ways the crate does that so every subsystem shares one
//! implementation instead of re-rolling a noop waker and a poll loop.
//!
//! # Cooperative mode (DST-safe)
//!
//! [`noop_waker`] plus [`block_on_cooperative`] drive a future to
//! completion by re-polling, never relying on a wakeup. Between
//! `Poll::Pending`s the caller's `idle` closure runs (a short sleep on
//! production threads, a spin-count assertion in tests), so forward
//! progress comes from work happening on *other* threads (io_uring
//! completion threads, the fabric progress thread) that the next poll
//! observes. This mode constructs no thread handle and reads no clock,
//! so it is the only mode that may run inside the deterministic
//! simulation executor.
//!
//! # Parked mode (production OS threads only)
//!
//! [`thread_waker`] plus [`park_block_on_until`] drive a future with a
//! *real* waker: `wake()` unparks the blocked thread, so a completion
//! posted from another thread resolves the wait immediately instead of
//! paying a fixed poll interval of latency on the fast path. This is
//! what the RPC worker pool uses while blocked on libfabric
//! completions.
//!
//! **NEVER use the parked mode in the deterministic simulation
//! executor.** It spawns nothing but it parks real OS threads and reads
//! the wall clock, both of which the DST forbids; the DST keeps the
//! cooperative mode.

use std::future::Future;
use std::sync::Arc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Wake, Waker};
use std::thread::{self, Thread};
use std::time::{Duration, Instant};

/// A [`Waker`] whose `wake`, `clone`, and `drop` are all no-ops. The
/// crate's single canonical noop waker: hand it to a [`Context`] when a
/// poll loop makes progress by re-polling rather than by being woken.
pub fn noop_waker() -> Waker {
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    // SAFETY: clone returns the same static vtable and wake/drop are
    // no-ops, so the waker carries no state; the data pointer is never
    // dereferenced.
    unsafe { Waker::from_raw(raw()) }
}

/// Drive `fut` to completion on the current thread with a [`noop_waker`],
/// running `idle` on every `Poll::Pending`.
///
/// Forward progress must come from elsewhere: the noop waker never
/// resolves the wait, so this only terminates once `fut` itself returns
/// `Poll::Ready` (because work on another thread completed). `idle` is
/// where the caller decides how to spend the gap between polls. A
/// production caller sleeps a few microseconds; a test passes a closure
/// that counts spins and asserts a bound so a stuck future fails loudly
/// instead of hanging.
///
/// This is the cooperative drive mode: it constructs no thread handle
/// and reads no clock of its own, so it is safe under the deterministic
/// simulation executor.
pub fn block_on_cooperative<Fut, Idle>(fut: Fut, mut idle: Idle) -> Fut::Output
where
    Fut: Future,
    Idle: FnMut(),
{
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut fut = std::pin::pin!(fut);
    loop {
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => idle(),
        }
    }
}

/// Waker that unparks a specific OS thread. A clone handed to a future
/// (and, via the completion registry, stored in its `AtomicWaker`)
/// unparks whichever thread constructed it.
struct ThreadWaker {
    thread: Thread,
}

impl Wake for ThreadWaker {
    fn wake(self: Arc<Self>) {
        self.thread.unpark();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        self.thread.unpark();
    }
}

/// A [`Waker`] that unparks the calling thread. Hand it to a `Context`
/// and `thread::park`/`park_timeout` between polls; any clone of it
/// unparks this same thread. Use when a caller must interleave its own
/// work between polls (the RPC worker writes a page between handler
/// stream polls) and so cannot delegate to [`park_block_on_until`].
///
/// Parked mode: real OS threads only, never under the DST.
pub fn thread_waker() -> Waker {
    Waker::from(Arc::new(ThreadWaker {
        thread: thread::current(),
    }))
}

/// Drive `fut` to completion on the current thread, parking between
/// polls, but abandon the wait early (returning `None`) once `timeout`
/// elapses or `interrupt` reports `true`. `interrupt` is consulted
/// before every poll, and each `Poll::Pending` parks for at most
/// `slice` (capped further by the remaining budget) so a caller blocked
/// on a completion that may never fire still observes an external
/// signal - server shutdown, say - within one `slice` even if nothing
/// unparks it. A wakeup that *does* arrive (the completion path unparks
/// via the thread waker) resolves immediately, so the fast path pays no
/// extra latency. `None` is returned for both the interrupt and the
/// timeout; the caller treats either as "give up". Pass a `slice` equal
/// to `timeout` and a `|| false` interrupt for a plain park-until-timeout.
///
/// Parked mode: real OS threads only, never under the DST.
///
/// # Wakeup race
///
/// `Thread::unpark` records a token if the thread is not currently
/// parked, so a completion that fires between a `Poll::Pending` and the
/// following `park`/`park_timeout` is not lost: the park returns
/// immediately and the next poll observes the stored result. Spurious
/// wakeups are equally harmless because every wakeup re-polls.
pub fn park_block_on_until<F, I>(
    fut: F,
    timeout: Duration,
    slice: Duration,
    mut interrupt: I,
) -> Option<F::Output>
where
    F: Future,
    I: FnMut() -> bool,
{
    let waker = thread_waker();
    let mut cx = Context::from_waker(&waker);
    let mut fut = std::pin::pin!(fut);
    let deadline = Instant::now() + timeout;
    loop {
        if interrupt() {
            return None;
        }
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return Some(v),
            Poll::Pending => {
                let now = Instant::now();
                if now >= deadline {
                    return None;
                }
                thread::park_timeout((deadline - now).min(slice));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::pin::Pin;
    use std::sync::Mutex;
    use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};

    #[test]
    fn cooperative_resolves_when_a_later_poll_is_ready() {
        // Pending for the first two polls, then ready: the idle closure
        // counts the gaps so we can assert it ran exactly twice.
        let polls = AtomicU32::new(0);
        let idle_runs = std::cell::Cell::new(0u32);
        let fut = std::future::poll_fn(|_cx| {
            if polls.fetch_add(1, Ordering::Relaxed) < 2 {
                Poll::Pending
            } else {
                Poll::Ready(99u32)
            }
        });
        let out = block_on_cooperative(fut, || idle_runs.set(idle_runs.get() + 1));
        assert_eq!(out, 99);
        assert_eq!(idle_runs.get(), 2);
    }

    #[test]
    fn cooperative_returns_ready_without_idling() {
        let idled = std::cell::Cell::new(false);
        let out = block_on_cooperative(std::future::ready(7u32), || idled.set(true));
        assert_eq!(out, 7);
        assert!(!idled.get(), "a ready future must not run the idle closure");
    }

    /// Future that resolves to `42` once `complete` is called, storing
    /// the polling waker so the completer can wake it from any thread.
    struct Signal {
        ready: Mutex<bool>,
        waker: Mutex<Option<Waker>>,
    }

    struct SignalFuture(Arc<Signal>);

    impl Future for SignalFuture {
        type Output = u32;

        fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<u32> {
            if *self.0.ready.lock().unwrap() {
                return Poll::Ready(42);
            }
            *self.0.waker.lock().unwrap() = Some(cx.waker().clone());
            Poll::Pending
        }
    }

    fn complete(sig: &Arc<Signal>) {
        *sig.ready.lock().unwrap() = true;
        if let Some(w) = sig.waker.lock().unwrap().take() {
            w.wake();
        }
    }

    #[test]
    fn ready_future_returns_without_parking() {
        let out = park_block_on_until(
            std::future::ready(7u32),
            Duration::from_secs(1),
            Duration::from_secs(1),
            || false,
        );
        assert_eq!(out, Some(7));
    }

    #[test]
    fn pending_future_times_out() {
        let out = park_block_on_until(
            std::future::pending::<u32>(),
            Duration::from_millis(10),
            Duration::from_millis(10),
            || false,
        );
        assert_eq!(out, None);
    }

    #[test]
    fn interrupt_abandons_a_pending_wait_before_timeout() {
        // A future that never completes, a generous timeout, but an
        // interrupt that is already set: the wait must give up at once
        // (return `None`) instead of parking out the full timeout.
        let out = park_block_on_until(
            std::future::pending::<u32>(),
            Duration::from_secs(3600),
            Duration::from_millis(5),
            || true,
        );
        assert_eq!(out, None);
    }

    #[test]
    fn interrupt_set_after_first_park_is_observed_within_a_slice() {
        // The interrupt flips true shortly after the wait parks; the
        // bounded slice guarantees the next loop iteration sees it and
        // returns `None` well inside the (effectively unbounded) timeout.
        let flag = Arc::new(AtomicBool::new(false));
        let setter = flag.clone();
        let handle = thread::spawn(move || {
            thread::sleep(Duration::from_millis(20));
            setter.store(true, Ordering::Release);
        });
        let out = park_block_on_until(
            std::future::pending::<u32>(),
            Duration::from_secs(3600),
            Duration::from_millis(5),
            || flag.load(Ordering::Acquire),
        );
        handle.join().unwrap();
        assert_eq!(out, None);
    }

    #[test]
    fn wakes_when_completed_from_another_thread() {
        let sig = Arc::new(Signal {
            ready: Mutex::new(false),
            waker: Mutex::new(None),
        });
        let completer = sig.clone();
        let handle = thread::spawn(move || {
            thread::sleep(Duration::from_millis(20));
            complete(&completer);
        });

        // A timeout far longer than the completer's delay: the only way
        // this resolves quickly is the wake/unpark path, not a poll
        // interval. A slice equal to the timeout proves the resolution
        // comes from the unpark, not from slice-bounded re-polling.
        let out = park_block_on_until(
            SignalFuture(sig),
            Duration::from_secs(5),
            Duration::from_secs(5),
            || false,
        );
        handle.join().unwrap();
        assert_eq!(out, Some(42));
    }
}
