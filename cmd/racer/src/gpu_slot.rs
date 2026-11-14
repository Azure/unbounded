//! The process-wide GPU helper slot.
//!
//! `RACER_ENABLE_GPU=auto` must not touch a GPU until a user asks for one, so
//! the helper starts on the first `gpu-read` request head instead of at
//! startup. [`GpuSlot::get`] never starts it; only [`GpuSlot::get_or_start`]
//! does, and only the `gpu-read` path calls that.
//!
//! One start attempt runs at a time on its own thread: concurrent requests
//! all wait on that attempt, and no I/O worker blocks on it. A started helper
//! is final. An attempt that fails or finds no usable GPU leaves the slot in
//! standby: the requests that waited on it get 501, and so does every
//! `gpu-read` until the backoff ([`retry_delay`]) has elapsed, when the next
//! `gpu-read` tries again. Only two cases turn GPU mode off for the life of
//! the process: a start that panics, and a start thread that fails to spawn.

use crate::gpu::GpuHelper;
use futures::channel::oneshot;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::OnceLock;
use std::time::Duration;
use std::time::Instant;

/// Starts the helper. `Ok(None)` and `Err` both mean this attempt failed.
pub type Starter = Box<dyn FnMut() -> crate::error::Result<Option<Arc<GpuHelper>>> + Send>;

/// The wait after the first failed start.
pub const RETRY_INITIAL: Duration = Duration::from_secs(5);

/// The longest wait between start attempts.
pub const RETRY_MAX: Duration = Duration::from_secs(60);

/// The wait after `failures` consecutive failed starts (at least one):
/// [`RETRY_INITIAL`], doubled per further failure, capped at [`RETRY_MAX`].
pub fn retry_delay(failures: u32) -> Duration {
    let doublings = failures.saturating_sub(1).min(16);
    RETRY_INITIAL.saturating_mul(1 << doublings).min(RETRY_MAX)
}

/// Whether a new attempt may start `elapsed` after the last of `failures`
/// consecutive failed starts. Always true before any failure.
pub fn retry_due(failures: u32, elapsed: Duration) -> bool {
    failures == 0 || elapsed >= retry_delay(failures)
}

/// Failed attempts so far, and when the last one ended.
#[derive(Clone, Copy, Default)]
struct Backoff {
    failures: u32,
    since: Option<Instant>,
}

/// Runs once with the started helper, before any waiter sees it.
pub type OnStart = Box<dyn FnOnce(&Arc<GpuHelper>) + Send>;

enum Pending {
    /// Not started yet, or the last attempt failed; holds what starts it.
    Idle(Starter, Option<OnStart>, Backoff),
    /// The start thread is running; these wait for it.
    Starting(Vec<oneshot::Sender<()>>, Backoff),
    /// Resolved; see `helper`.
    Done,
}

/// Shared with the start thread.
struct Inner {
    helper: OnceLock<Option<Arc<GpuHelper>>>,
    pending: Mutex<Pending>,
    /// Scales [`retry_delay`]; 1 outside tests.
    #[cfg(test)]
    backoff_scale: f64,
}

impl Inner {
    fn delay(&self, failures: u32) -> Duration {
        let delay = retry_delay(failures);
        #[cfg(test)]
        let delay = delay.mul_f64(self.backoff_scale);
        delay
    }

    /// Return a failed attempt's starter to standby and wake its waiters,
    /// who see no helper.
    fn fail(&self, start: Starter, on_start: Option<OnStart>, backoff: Backoff) {
        let waiters = {
            let mut pending = self.lock();
            let waiters = match std::mem::replace(&mut *pending, Pending::Done) {
                Pending::Starting(waiters, _) => waiters,
                _ => Vec::new(),
            };
            *pending = Pending::Idle(start, on_start, backoff);
            waiters
        };
        for waiter in waiters {
            let _ = waiter.send(());
        }
    }

    /// Publish the outcome, then wake every waiter.
    fn finish(&self, helper: Option<Arc<GpuHelper>>) {
        let _ = self.helper.set(helper);
        let waiters = match std::mem::replace(&mut *self.lock(), Pending::Done) {
            Pending::Starting(waiters, _) => waiters,
            _ => Vec::new(),
        };
        for waiter in waiters {
            let _ = waiter.send(());
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Pending> {
        self.pending
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

/// Owned by the start thread; publishes "off" on drop unless finished.
struct Resolve(Option<Arc<Inner>>);

impl Resolve {
    fn finish(mut self, helper: Option<Arc<GpuHelper>>) {
        if let Some(inner) = self.0.take() {
            inner.finish(helper);
        }
    }
}

impl Drop for Resolve {
    fn drop(&mut self) {
        if let Some(inner) = self.0.take() {
            eprintln!("racer: stage=gpu state=disabled reason=start-panicked");
            inner.finish(None);
        }
    }
}

/// The process-wide GPU helper, started now or on first use.
pub struct GpuSlot {
    inner: Arc<Inner>,
}

impl Default for GpuSlot {
    /// GPU mode off.
    fn default() -> Self {
        Self::ready(None)
    }
}

impl GpuSlot {
    /// A slot whose outcome is already known (eager start, off, or tests).
    pub fn ready(helper: Option<Arc<GpuHelper>>) -> Self {
        Self {
            inner: Arc::new(Inner {
                helper: OnceLock::from(helper),
                pending: Mutex::new(Pending::Done),
                #[cfg(test)]
                backoff_scale: 1.0,
            }),
        }
    }

    /// A slot that runs `start` on the first [`Self::get_or_start`], then
    /// `on_start` with the helper when one started. A failed attempt is
    /// retried by a later [`Self::get_or_start`] after the backoff.
    pub fn lazy(start: Starter, on_start: OnStart) -> Self {
        Self {
            inner: Arc::new(Inner {
                helper: OnceLock::new(),
                pending: Mutex::new(Pending::Idle(start, Some(on_start), Backoff::default())),
                #[cfg(test)]
                backoff_scale: 1.0,
            }),
        }
    }

    /// [`Self::lazy`] with the backoff scaled by `scale`.
    #[cfg(test)]
    pub(crate) fn lazy_scaled(start: Starter, on_start: OnStart, scale: f64) -> Self {
        let slot = Self::lazy(start, on_start);
        let inner = Arc::into_inner(slot.inner).expect("fresh slot");
        Self {
            inner: Arc::new(Inner {
                backoff_scale: scale,
                ..inner
            }),
        }
    }

    /// The helper, if it has started. Never starts it.
    pub fn get(&self) -> Option<&Arc<GpuHelper>> {
        self.inner.helper.get().and_then(Option::as_ref)
    }

    /// Whether the slot still waits for a `gpu-read` to start the helper:
    /// before the first attempt, or after failed ones.
    pub fn standby(&self) -> bool {
        self.inner.helper.get().is_none()
    }

    /// The helper, starting it on first use. Waits without blocking the
    /// calling thread; concurrent callers share one start.
    pub async fn get_or_start(&self) -> Option<Arc<GpuHelper>> {
        if let Some(helper) = self.inner.helper.get() {
            return helper.clone();
        }
        let waiter = {
            let mut pending = self.inner.lock();
            // Checked under the lock: `finish` publishes before it takes the
            // lock to drain waiters, so no waiter is missed.
            if let Some(helper) = self.inner.helper.get() {
                return helper.clone();
            }
            let (sender, receiver) = oneshot::channel();
            match std::mem::replace(&mut *pending, Pending::Done) {
                Pending::Idle(start, on_start, backoff) => {
                    let elapsed = backoff
                        .since
                        .map(|since| since.elapsed())
                        .unwrap_or_default();
                    if backoff.failures > 0 && elapsed < self.inner.delay(backoff.failures) {
                        // Still backing off: this request gets 501.
                        *pending = Pending::Idle(start, on_start, backoff);
                        return None;
                    }
                    *pending = Pending::Starting(vec![sender], backoff);
                    drop(pending);
                    self.spawn(start, on_start, backoff);
                }
                Pending::Starting(mut waiters, backoff) => {
                    // Drop waiters whose request gave up, so a stalled
                    // start does not grow this without bound.
                    waiters.retain(|waiter| !waiter.is_canceled());
                    waiters.push(sender);
                    *pending = Pending::Starting(waiters, backoff);
                }
                // Done is only set together with `helper`; unreachable.
                Pending::Done => return None,
            }
            receiver
        };
        let _ = waiter.await;
        self.inner.helper.get().cloned().flatten()
    }

    /// Run the start on its own thread: it blocks on driver calls.
    fn spawn(&self, mut start: Starter, on_start: Option<OnStart>, backoff: Backoff) {
        let inner = self.inner.clone();
        let attempt = backoff.failures.saturating_add(1);
        eprintln!("racer: stage=gpu state=starting attempt={attempt}");
        let spawned = std::thread::Builder::new()
            .name("racer-gpu-start".into())
            .spawn(move || {
                // Resolves the slot as off if `start` or `on_start` unwinds,
                // so waiters never wait on a start that will not finish.
                let mut resolve = Resolve(Some(inner.clone()));
                let outcome = start();
                let reason = match outcome {
                    Ok(Some(helper)) => {
                        if let Some(on_start) = on_start {
                            on_start(&helper);
                        }
                        if attempt > 1 {
                            eprintln!("racer: stage=gpu state=recovered attempt={attempt}");
                        }
                        resolve.finish(Some(helper));
                        return;
                    }
                    Ok(None) => "no-usable-gpu".to_string(),
                    Err(error) => error.to_string(),
                };
                let delay = inner.delay(attempt);
                eprintln!(
                    "racer: stage=gpu state=start-failed attempt={attempt} reason={reason} retry_in={}s",
                    delay.as_secs_f64()
                );
                let backoff = Backoff {
                    failures: attempt,
                    since: Some(Instant::now()),
                };
                // Defuse the panic guard: the slot goes back to standby.
                resolve.0 = None;
                inner.fail(start, on_start, backoff);
            });
        if spawned.is_err() {
            eprintln!("racer: stage=gpu state=disabled reason=spawn-failed");
            self.inner.finish(None);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::future::Future;
    use std::sync::atomic::AtomicUsize;
    use std::sync::atomic::Ordering;

    /// A lazy slot whose start counts its calls, waits for `gate`, and
    /// returns `helper`.
    fn counted(
        calls: &Arc<AtomicUsize>,
        gate: std::sync::mpsc::Receiver<()>,
        helper: Option<Arc<GpuHelper>>,
    ) -> GpuSlot {
        let calls = calls.clone();
        GpuSlot::lazy(
            Box::new(move || {
                calls.fetch_add(1, Ordering::SeqCst);
                let _ = gate.recv();
                Ok(helper.clone())
            }),
            Box::new(|_| {}),
        )
    }

    #[test]
    /// Reading a lazy slot never starts it.
    fn get_never_starts() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (_open, gate) = std::sync::mpsc::channel();
        let slot = counted(&calls, gate, None);
        for _ in 0..3 {
            assert!(slot.get().is_none());
            assert!(slot.standby());
        }
        assert_eq!(calls.load(Ordering::SeqCst), 0);
    }

    #[test]
    /// Concurrent first callers wait on one start and see the same helper.
    fn concurrent_callers_share_one_start() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (open, gate) = std::sync::mpsc::channel();
        let helper = crate::gpu::testing::helper(&[None]);
        let slot = Arc::new(counted(&calls, gate, Some(helper.clone())));
        let waiters: Vec<_> = (0..8)
            .map(|_| {
                let slot = slot.clone();
                std::thread::spawn(move || futures::executor::block_on(slot.get_or_start()))
            })
            .collect();
        // Let every waiter queue up behind the one start.
        std::thread::sleep(std::time::Duration::from_millis(50));
        assert!(slot.standby());
        open.send(()).unwrap();
        for waiter in waiters {
            let got = waiter.join().unwrap().unwrap();
            assert!(Arc::ptr_eq(&got, &helper));
        }
        assert_eq!(calls.load(Ordering::SeqCst), 1);
        assert!(Arc::ptr_eq(slot.get().unwrap(), &helper));
        assert!(!slot.standby());
    }

    #[test]
    /// A start that fails or finds nothing returns the slot to standby, and
    /// requests within the backoff do not start it again.
    fn failure_returns_to_standby() {
        for result in [Ok(None), Err(crate::error::Error::Unavailable)] {
            let calls = Arc::new(AtomicUsize::new(0));
            let counted = calls.clone();
            let hooked = Arc::new(AtomicUsize::new(0));
            let hook = hooked.clone();
            let mut result = Some(result);
            let slot = GpuSlot::lazy(
                Box::new(move || {
                    counted.fetch_add(1, Ordering::SeqCst);
                    result.take().unwrap_or(Ok(None))
                }),
                Box::new(move |_| {
                    hook.fetch_add(1, Ordering::SeqCst);
                }),
            );
            for _ in 0..3 {
                assert!(futures::executor::block_on(slot.get_or_start()).is_none());
            }
            assert_eq!(calls.load(Ordering::SeqCst), 1);
            assert_eq!(hooked.load(Ordering::SeqCst), 0);
            assert!(slot.standby());
            assert!(matches!(
                &*slot.inner.lock(),
                Pending::Idle(_, Some(_), Backoff { failures: 1, .. })
            ));
        }
    }

    #[test]
    /// Once the backoff elapses, the next request retries; a success after
    /// failures is final and runs the start hook once.
    fn retry_after_backoff_recovers() {
        let calls = Arc::new(AtomicUsize::new(0));
        let counted = calls.clone();
        let hooked = Arc::new(AtomicUsize::new(0));
        let hook = hooked.clone();
        let helper = crate::gpu::testing::helper(&[None]);
        let started = helper.clone();
        // 5 s scaled to 5 ms, 10 s to 10 ms.
        let slot = GpuSlot::lazy_scaled(
            Box::new(move || match counted.fetch_add(1, Ordering::SeqCst) {
                0 => Err(crate::error::Error::Unavailable),
                1 => Ok(None),
                _ => Ok(Some(started.clone())),
            }),
            Box::new(move |_| {
                hook.fetch_add(1, Ordering::SeqCst);
            }),
            0.001,
        );
        for attempt in 1..=3 {
            let mut got = None;
            for _ in 0..1000 {
                got = futures::executor::block_on(slot.get_or_start());
                if calls.load(Ordering::SeqCst) == attempt {
                    break;
                }
                std::thread::sleep(Duration::from_millis(1));
            }
            assert_eq!(calls.load(Ordering::SeqCst), attempt);
            assert_eq!(got.is_some(), attempt == 3);
        }
        assert!(Arc::ptr_eq(slot.get().unwrap(), &helper));
        assert!(!slot.standby());
        assert_eq!(hooked.load(Ordering::SeqCst), 1);
        for _ in 0..3 {
            assert!(futures::executor::block_on(slot.get_or_start()).is_some());
        }
        assert_eq!(calls.load(Ordering::SeqCst), 3);
    }

    #[test]
    fn retry_delay_doubles_to_ceiling() {
        let delays: Vec<u64> = (1..=8).map(|n| retry_delay(n).as_secs()).collect();
        assert_eq!(delays, [5, 10, 20, 40, 60, 60, 60, 60]);
        assert_eq!(retry_delay(0), RETRY_INITIAL);
        assert_eq!(retry_delay(u32::MAX), RETRY_MAX);
    }

    #[test]
    fn retry_due_waits_for_delay() {
        assert!(retry_due(0, Duration::ZERO));
        assert!(!retry_due(1, Duration::from_millis(4999)));
        assert!(retry_due(1, Duration::from_secs(5)));
        assert!(!retry_due(3, Duration::from_secs(19)));
        assert!(retry_due(3, Duration::from_secs(20)));
        assert!(!retry_due(10, Duration::from_secs(59)));
        assert!(retry_due(10, Duration::from_secs(60)));
    }

    #[test]
    /// The start hook runs once, before any waiter sees the helper.
    fn on_start_runs_before_waiters_resume() {
        let helper = crate::gpu::testing::helper(&[None]);
        let hooked = Arc::new(AtomicUsize::new(0));
        let hook = hooked.clone();
        let started = helper.clone();
        let slot = GpuSlot::lazy(
            Box::new(move || Ok(Some(started.clone()))),
            Box::new(move |_| {
                std::thread::sleep(std::time::Duration::from_millis(20));
                hook.fetch_add(1, Ordering::SeqCst);
            }),
        );
        assert!(futures::executor::block_on(slot.get_or_start()).is_some());
        assert_eq!(hooked.load(Ordering::SeqCst), 1);
        assert!(futures::executor::block_on(slot.get_or_start()).is_some());
        assert_eq!(hooked.load(Ordering::SeqCst), 1);
    }

    #[test]
    /// Waiters that give up during a stalled start are pruned, and a live
    /// waiter still gets the result.
    fn canceled_waiters_are_pruned() {
        let calls = Arc::new(AtomicUsize::new(0));
        let (open, gate) = std::sync::mpsc::channel();
        let helper = crate::gpu::testing::helper(&[None]);
        let slot = counted(&calls, gate, Some(helper.clone()));
        let waiting = |slot: &GpuSlot| match &*slot.inner.lock() {
            Pending::Starting(waiters, _) => waiters.len(),
            _ => panic!("start is not pending"),
        };
        for _ in 0..100 {
            let mut request = Box::pin(slot.get_or_start());
            let waker = futures::task::noop_waker();
            let mut cx = std::task::Context::from_waker(&waker);
            assert!(request.as_mut().poll(&mut cx).is_pending());
            // Dropping the request cancels its waiter.
            drop(request);
            assert!(waiting(&slot) <= 1);
        }
        let mut live = Box::pin(slot.get_or_start());
        let waker = futures::task::noop_waker();
        let mut cx = std::task::Context::from_waker(&waker);
        assert!(live.as_mut().poll(&mut cx).is_pending());
        assert_eq!(waiting(&slot), 1);
        open.send(()).unwrap();
        let got = futures::executor::block_on(live).unwrap();
        assert!(Arc::ptr_eq(&got, &helper));
        assert_eq!(calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    /// A start or start hook that panics resolves the slot as off and wakes
    /// every waiter instead of leaving it starting forever.
    fn panicking_start_resolves_slot_and_wakes_waiters() {
        type Case = (Starter, OnStart);
        let cases: Vec<fn() -> Case> = vec![
            || {
                (
                    Box::new(|| panic!("simulated start panic")),
                    Box::new(|_| {}),
                )
            },
            || {
                (
                    Box::new(|| Ok(Some(crate::gpu::testing::helper(&[None])))),
                    Box::new(|_| panic!("simulated on_start panic")),
                )
            },
        ];
        for case in cases {
            let (start, on_start) = case();
            let slot = Arc::new(GpuSlot::lazy(start, on_start));
            let waiters: Vec<_> = (0..4)
                .map(|_| {
                    let slot = slot.clone();
                    std::thread::spawn(move || futures::executor::block_on(slot.get_or_start()))
                })
                .collect();
            for waiter in waiters {
                assert!(waiter.join().unwrap().is_none());
            }
            assert!(!slot.standby());
            assert!(slot.get().is_none());
            assert!(matches!(&*slot.inner.lock(), Pending::Done));
            assert!(futures::executor::block_on(slot.get_or_start()).is_none());
        }
    }

    #[test]
    fn ready_slots_are_resolved() {
        let off = GpuSlot::default();
        assert!(!off.standby());
        assert!(off.get().is_none());
        assert!(futures::executor::block_on(off.get_or_start()).is_none());
        let helper = crate::gpu::testing::helper(&[None]);
        let on = GpuSlot::ready(Some(helper.clone()));
        assert!(Arc::ptr_eq(on.get().unwrap(), &helper));
    }
}
