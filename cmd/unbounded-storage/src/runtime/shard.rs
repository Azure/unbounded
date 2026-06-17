// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-shard cooperative future-set driver.
//!
//! `ShardLoop` owns the set of futures that run on a single shard's
//! pinned OS thread and drives them cooperatively, mirroring the
//! cooperative loop discipline used by `bin/bench.rs` and the disk
//! supervisor's `run_disk_thread`. The crate is runtime agnostic by
//! design, so this does not pull in tokio or any other executor.
//!
//! The registered future set is the extension point for the
//! shard's socket-ring, frontend, backend, and pool-consumer
//! futures. Fabric and Pool self-drive progress via their own
//! internal progress threads, so the loop only needs to poll the
//! registered futures; it does not poke those subsystems directly.
//!
//! Non-future progress sources (notably the per-shard
//! [`NetworkRing`](crate::ring::NetworkRing)) are driven via
//! *tick hooks*: closures invoked once per iteration that report
//! whether they did work. When any hook or future makes progress the
//! loop spins again immediately; only a fully idle iteration sleeps the
//! idle interval, so socket I/O is busy-polled while active and parked
//! cheaply when quiet.
//!
//! Cross-thread disk replies are the one progress source that is
//! neither a tick hook nor visible to a synchronous poll: a page
//! read/write completes on a *storage core* thread, which calls
//! [`ReplySlot::set`](crate::storage::ReplySlot) ->
//! `Waker::wake`. To make that wake observable on the shard thread,
//! the futures are polled not with a noop waker but with a small
//! flag-flipping waker (see [`flag_waker`]): its `wake` sets a shared
//! `Arc<AtomicBool>` that the idle-park logic checks before sleeping.
//! A freshly-set flag is treated as "busy this iteration", so a
//! landed disk reply triggers a near-immediate re-poll instead of
//! waiting out the full idle interval. When nothing is pending the
//! flag stays clear and the loop still parks the idle interval, so
//! this never degrades into a busy-spin.

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};
use std::thread;
use std::time::Duration;

/// Default *idle* sleep between [`ShardLoop`] iterations once nothing
/// made progress. Matches the shard's shutdown poll interval in
/// `main.rs` (`SHUTDOWN_POLL`). When an iteration is busy the loop does
/// not sleep at all.
const DEFAULT_POLL: Duration = Duration::from_millis(100);

/// Cooperative driver for a single shard's future set. Owns the
/// registered futures, the tick hooks that drive non-future progress
/// sources, and the flag-flipping waker used to poll the futures.
pub struct ShardLoop {
    futures: Vec<Pin<Box<dyn Future<Output = ()>>>>,
    /// Per-iteration progress sources (e.g. the socket ring's
    /// `progress()`). Each returns whether it did work this tick.
    /// Deliberately not `Send`: the loop runs on the pinned shard
    /// thread and hooks close over `!Send` resources like the ring.
    tick_hooks: Vec<Box<dyn FnMut() -> bool>>,
    waker: Waker,
    /// Set to `true` by `waker`'s `wake` when a future polled on this
    /// shard is woken from another thread (the storage core's
    /// [`ReplySlot::set`](crate::storage::ReplySlot) reply path). The
    /// idle-park logic observes and clears it so a landed disk reply
    /// re-polls immediately instead of parking the full idle interval.
    /// Shares its backing allocation with `waker`.
    wake_flag: Arc<AtomicBool>,
}

impl ShardLoop {
    pub fn new() -> Self {
        let (waker, wake_flag) = flag_waker();
        Self {
            futures: Vec::new(),
            tick_hooks: Vec::new(),
            waker,
            wake_flag,
        }
    }

    /// Register a future to be polled by [`tick`](Self::tick). The
    /// future runs until it returns `Poll::Ready`, at which point it
    /// is dropped from the set.
    pub fn spawn<F: Future<Output = ()> + 'static>(&mut self, fut: F) {
        self.futures.push(Box::pin(fut));
    }

    /// Register a tick hook: a closure invoked once per loop iteration
    /// (before futures are polled) that returns `true` when it did
    /// work. Used to drive non-future progress sources such as the
    /// per-shard socket ring's `progress()`.
    pub fn add_tick_hook<F: FnMut() -> bool + 'static>(&mut self, hook: F) {
        self.tick_hooks.push(Box::new(hook));
    }

    /// A clone of the shard's flag-flipping waker (see [`flag_waker`]).
    ///
    /// Tick hooks that poll their own futures (rather than spawning them
    /// onto the loop) should poll with this waker instead of a noop one
    /// so a cross-thread completion (a disk reply or fabric event landing
    /// on another core) sets the shard's `wake_flag` and suppresses the
    /// next idle park. Without it such a hook would have to report itself
    /// perpetually busy to force re-polling, which busy-spins the shard
    /// thread and starves co-located threads on a CPU-constrained host.
    pub fn waker(&self) -> Waker {
        self.waker.clone()
    }

    /// Run every tick hook then poll every live future once, dropping
    /// any future that completed. Returns whether the iteration was
    /// *busy*: true if any tick hook reported work (futures completing
    /// is part of normal drain and does not by itself keep the loop
    /// hot). This is the internal driver used by
    /// [`run_until_with`](Self::run_until_with).
    ///
    /// A future woken from another thread (a disk reply landing on a
    /// storage core) does not show up in this `busy` flag; it is
    /// surfaced separately via the `wake_flag`, which
    /// `run_until_with` checks before parking.
    fn drive(&mut self) -> bool {
        let mut busy = false;
        for hook in &mut self.tick_hooks {
            if hook() {
                busy = true;
            }
        }
        let mut cx = Context::from_waker(&self.waker);
        let mut i = 0;
        while i < self.futures.len() {
            match self.futures[i].as_mut().poll(&mut cx) {
                Poll::Ready(()) => {
                    let _ = self.futures.swap_remove(i);
                }
                Poll::Pending => {
                    i += 1;
                }
            }
        }
        busy
    }

    /// Poll every live future exactly once with the noop context and
    /// drop any that completed this tick. Tick hooks are also invoked.
    /// Retained for callers/tests that just want to advance one
    /// iteration without observing the busy flag.
    pub fn tick(&mut self) {
        let _ = self.drive();
    }

    /// Drive the future set until `should_stop` returns true.
    ///
    /// Each iteration runs tick hooks and polls every live future once
    /// via the internal driver, checks `should_stop`, and otherwise
    /// sleeps [`DEFAULT_POLL`] before the next pass *only when the
    /// iteration was idle*. Fabric and Pool self-drive their own
    /// progress, so this loop only advances the registered futures and
    /// hooks.
    pub fn run_until<F: FnMut() -> bool>(&mut self, should_stop: F) {
        self.run_until_with(should_stop, DEFAULT_POLL);
    }

    /// Like [`run_until`](Self::run_until) but with an explicit *idle*
    /// poll interval. A busy iteration (any tick hook did work) loops
    /// again immediately without sleeping so socket I/O is busy-polled;
    /// only a fully idle iteration sleeps `poll` before re-checking
    /// `should_stop`. Tests pass a zero interval to drive the loop
    /// without sleeping for real time.
    ///
    /// A cross-thread disk reply that landed around this iteration is
    /// also treated as busy: the reply path wakes the shard's
    /// flag-flipping `waker`, setting the `wake_flag`. The flag is
    /// observed and cleared here, and a set flag suppresses the sleep
    /// so the next iteration re-polls the completed future immediately
    /// rather than waiting out the idle interval. When no reply landed
    /// the flag stays clear and the loop parks `poll` as before, so a
    /// quiet shard never busy-spins.
    pub fn run_until_with<F: FnMut() -> bool>(&mut self, mut should_stop: F, poll: Duration) {
        loop {
            let busy = self.drive();
            if should_stop() {
                return;
            }
            // Observe and clear any cross-thread wake. A set flag means
            // a disk reply was delivered around this poll, so skip the
            // park and re-poll immediately on the next pass.
            let woken = self.wake_flag.swap(false, Ordering::AcqRel);
            if !busy && !woken {
                thread::sleep(poll);
            }
        }
    }

    /// True when no futures remain live.
    pub fn is_empty(&self) -> bool {
        self.futures.is_empty()
    }

    /// Number of futures still live (not yet `Poll::Ready`).
    pub fn len(&self) -> usize {
        self.futures.len()
    }
}

impl Default for ShardLoop {
    fn default() -> Self {
        Self::new()
    }
}

/// Build a `Waker` whose `wake` flips a shared `Arc<AtomicBool>` to
/// `true`, returning the waker and a clone of that flag. The shard
/// polls its futures with this waker so a cross-thread
/// [`ReplySlot::set`](crate::storage::ReplySlot) -> `Waker::wake` on
/// the storage core is observable on the shard thread (under a noop
/// waker it would be a silent no-op). `clone`/`wake`/`drop` follow the
/// standard `Arc` refcount discipline so the flag outlives every waker
/// clone the futures may stash.
fn flag_waker() -> (Waker, Arc<AtomicBool>) {
    let flag = Arc::new(AtomicBool::new(false));
    let data = Arc::into_raw(flag.clone()) as *const ();
    // SAFETY: `data` is a freshly leaked `Arc<AtomicBool>` pointer and
    // `FLAG_VTABLE` upholds the matching clone/wake/drop refcounting.
    let waker = unsafe { Waker::from_raw(RawWaker::new(data, &FLAG_VTABLE)) };
    (waker, flag)
}

static FLAG_VTABLE: RawWakerVTable =
    RawWakerVTable::new(flag_clone, flag_wake, flag_wake_by_ref, flag_drop);

unsafe fn flag_clone(data: *const ()) -> RawWaker {
    // SAFETY: `data` points at a live `Arc<AtomicBool>`; bumping the
    // strong count balances the `drop` the cloned waker will perform.
    unsafe { Arc::increment_strong_count(data as *const AtomicBool) };
    RawWaker::new(data, &FLAG_VTABLE)
}

unsafe fn flag_wake(data: *const ()) {
    // `wake` consumes the waker: reclaim its strong ref, flip the
    // flag, then drop the ref as `arc` falls out of scope.
    // SAFETY: `data` came from `Arc::into_raw` and this consumes that
    // owned ref exactly once.
    let arc = unsafe { Arc::from_raw(data as *const AtomicBool) };
    arc.store(true, Ordering::Release);
}

unsafe fn flag_wake_by_ref(data: *const ()) {
    // `wake_by_ref` does not consume the waker: borrow the flag
    // without taking ownership of the strong ref.
    // SAFETY: `data` points at a live `Arc<AtomicBool>`; we hand the
    // reclaimed ref straight back via `into_raw` so the count is
    // unchanged.
    let arc = unsafe { Arc::from_raw(data as *const AtomicBool) };
    arc.store(true, Ordering::Release);
    let _ = Arc::into_raw(arc);
}

unsafe fn flag_drop(data: *const ()) {
    // SAFETY: balances one `clone`/`into_raw` strong ref.
    unsafe { Arc::decrement_strong_count(data as *const AtomicBool) };
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::{Cell, RefCell};
    use std::rc::Rc;
    use std::task::Context as TaskContext;

    /// Captures the shard's waker on first poll then parks forever.
    /// Stands in for a page-channel reply future: the captured waker
    /// is the one a storage core's `ReplySlot::set` would later wake.
    struct CaptureWaker {
        slot: Rc<RefCell<Option<Waker>>>,
    }

    impl Future for CaptureWaker {
        type Output = ();
        fn poll(self: Pin<&mut Self>, cx: &mut TaskContext<'_>) -> Poll<()> {
            *self.slot.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        }
    }

    /// Yields `Poll::Pending` `n` times before completing, so the
    /// loop has to make multiple passes to drain it.
    struct YieldN {
        remaining: usize,
    }

    impl Future for YieldN {
        type Output = ();
        fn poll(mut self: Pin<&mut Self>, _cx: &mut TaskContext<'_>) -> Poll<()> {
            if self.remaining == 0 {
                Poll::Ready(())
            } else {
                self.remaining -= 1;
                Poll::Pending
            }
        }
    }

    #[test]
    fn tick_drains_futures_to_completion() {
        let done = Rc::new(Cell::new(0u32));
        let mut sl = ShardLoop::new();
        for n in [0usize, 1, 3] {
            let done = done.clone();
            sl.spawn(async move {
                YieldN { remaining: n }.await;
                done.set(done.get() + 1);
            });
        }
        assert_eq!(sl.len(), 3);

        // The slowest future yields 3 times, so it is drained within
        // a bounded number of ticks. The bound fails loudly rather
        // than hanging if a future never completes.
        let mut ticks = 0;
        while !sl.is_empty() {
            sl.tick();
            ticks += 1;
            assert!(ticks < 1000, "tick did not drain futures within bound");
        }
        assert_eq!(done.get(), 3);
        assert!(sl.is_empty());
    }

    #[test]
    fn run_until_exits_and_drains() {
        let mut sl = ShardLoop::new();
        sl.spawn(async {
            YieldN { remaining: 2 }.await;
        });

        // Stop after a handful of ticks; the future (2 yields) is
        // drained well before the stop flips.
        let ticks = Cell::new(0u32);
        sl.run_until_with(
            || {
                let n = ticks.get() + 1;
                ticks.set(n);
                n >= 5
            },
            Duration::from_millis(0),
        );
        assert!(ticks.get() >= 5);
        assert!(sl.is_empty());
    }

    #[test]
    fn tick_hook_invoked_each_iteration() {
        let calls = Rc::new(Cell::new(0u32));
        let mut sl = ShardLoop::new();
        {
            let calls = calls.clone();
            // A hook that never reports work, so the loop stays idle.
            sl.add_tick_hook(move || {
                calls.set(calls.get() + 1);
                false
            });
        }
        let ticks = Cell::new(0u32);
        sl.run_until_with(
            || {
                let n = ticks.get() + 1;
                ticks.set(n);
                n >= 4
            },
            Duration::from_millis(0),
        );
        // The hook runs once per iteration; the loop ran `ticks`
        // iterations before stopping.
        assert_eq!(calls.get(), ticks.get());
        assert!(calls.get() >= 4);
    }

    #[test]
    fn busy_hook_does_not_sleep_and_drains_on_stop() {
        // A hook that reports busy for its first few calls then goes
        // idle. While busy the loop must spin without sleeping; a
        // non-zero idle interval makes a sleep observable as wall time.
        let busy_budget = Rc::new(Cell::new(3u32));
        let calls = Rc::new(Cell::new(0u32));
        let mut sl = ShardLoop::new();
        {
            let busy_budget = busy_budget.clone();
            let calls = calls.clone();
            sl.add_tick_hook(move || {
                calls.set(calls.get() + 1);
                let b = busy_budget.get();
                if b > 0 {
                    busy_budget.set(b - 1);
                    true
                } else {
                    false
                }
            });
        }
        // Also register a future so we can assert the loop drains it.
        let done = Rc::new(Cell::new(false));
        {
            let done = done.clone();
            sl.spawn(async move {
                YieldN { remaining: 1 }.await;
                done.set(true);
            });
        }

        let start = std::time::Instant::now();
        // Stop only after the hook has gone idle (calls > busy span),
        // so the loop must traverse the busy iterations first.
        sl.run_until_with(|| calls.get() >= 6, Duration::from_millis(50));
        let elapsed = start.elapsed();

        // Three busy iterations spun without sleeping; only the idle
        // ones could sleep. The future drained, and the loop exited
        // once the stop condition flipped.
        assert!(done.get(), "registered future drained");
        assert!(calls.get() >= 6);
        // While busy the loop did not sleep 50ms per iteration. Three
        // busy iterations alone would otherwise have cost 150ms; the
        // generous bound just guards against sleeping on every tick.
        assert!(
            elapsed < Duration::from_millis(150),
            "busy iterations should not sleep: elapsed={elapsed:?}"
        );
    }

    /// The flag-flipping waker that the shard polls with must set its
    /// shared flag on `wake`, and clones (as `ReplySlot` stash) and
    /// `wake_by_ref` must target the same flag. This is the wiring the
    /// cross-thread reply path relies on.
    #[test]
    fn flag_waker_wake_sets_shared_flag() {
        let (waker, flag) = flag_waker();
        assert!(!flag.load(Ordering::Acquire));

        // A clone targets the same flag, mirroring the waker a
        // `ReplySlot` clones and stores; consuming `wake` flips it.
        let cloned = waker.clone();
        cloned.wake();
        assert!(flag.load(Ordering::Acquire), "wake must set the flag");

        // `wake_by_ref` (non-consuming) flips it too.
        flag.store(false, Ordering::Release);
        waker.wake_by_ref();
        assert!(
            flag.load(Ordering::Acquire),
            "wake_by_ref must set the flag"
        );
    }

    /// A cross-thread wake (a disk reply landing, simulated by waking
    /// the captured shard waker) must make the next idle iteration
    /// re-poll immediately instead of parking the idle interval.
    #[test]
    fn remote_wake_skips_idle_park() {
        let mut sl = ShardLoop::new();
        let slot: Rc<RefCell<Option<Waker>>> = Rc::new(RefCell::new(None));
        sl.spawn(CaptureWaker { slot: slot.clone() });

        // One drive captures the shard's waker into the slot, the way a
        // page-channel reply future stashes it in its `ReplySlot`.
        sl.tick();
        let waker = slot.borrow().clone().expect("waker captured on first poll");

        // Simulate the storage core's `ReplySlot::set` -> `Waker::wake`.
        waker.wake();

        // A 10s idle interval would dominate the elapsed time if the
        // wake were lost; the set flag must suppress that one park.
        let ticks = Cell::new(0u32);
        let start = std::time::Instant::now();
        sl.run_until_with(
            || {
                let n = ticks.get() + 1;
                ticks.set(n);
                n >= 2
            },
            Duration::from_secs(10),
        );
        let elapsed = start.elapsed();
        assert!(
            elapsed < Duration::from_secs(1),
            "a landed reply must skip the idle park: elapsed={elapsed:?}"
        );
    }

    /// Guard against the busy-spin regression: with nothing pending and
    /// no wake, a fully idle iteration must still park the interval.
    #[test]
    fn idle_without_wake_still_parks() {
        let mut sl = ShardLoop::new();
        let ticks = Cell::new(0u32);
        let start = std::time::Instant::now();
        // iter 1 is idle (no hooks, no futures, no wake) and must park
        // `poll`; iter 2 trips the stop. Elapsed therefore covers one
        // full park.
        sl.run_until_with(
            || {
                let n = ticks.get() + 1;
                ticks.set(n);
                n >= 2
            },
            Duration::from_millis(40),
        );
        let elapsed = start.elapsed();
        assert!(
            elapsed >= Duration::from_millis(30),
            "an idle shard must still park, not busy-spin: elapsed={elapsed:?}"
        );
    }
}
