//! The process-wide GPU helper slot.
//!
//! `RACER_ENABLE_GPU=auto` must not touch a GPU until a user asks for one, so
//! the helper starts on the first `gpu-read` request head instead of at
//! startup. [`GpuSlot::get`] never starts it; only [`GpuSlot::get_or_start`]
//! does, and only the `gpu-read` path calls that.
//!
//! The start runs once per process on its own thread: concurrent first
//! requests all wait on that one start, and no I/O worker blocks on it. The
//! outcome is final. A start that fails or finds no usable GPU leaves GPU
//! mode off for the life of the process; it is not retried, so a bad node
//! pays the probe cost once and every later `gpu-read` gets 501 at once.

use crate::gpu::GpuHelper;
use futures::channel::oneshot;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::OnceLock;

/// Starts the helper. `Ok(None)` and `Err` both mean GPU mode is off.
pub type Starter = Box<dyn FnOnce() -> crate::error::Result<Option<Arc<GpuHelper>>> + Send>;

/// Runs once with the started helper, before any waiter sees it.
pub type OnStart = Box<dyn FnOnce(&Arc<GpuHelper>) + Send>;

enum Pending {
    /// Not started yet; holds what starts it.
    Idle(Starter, Option<OnStart>),
    /// The start thread is running; these wait for it.
    Starting(Vec<oneshot::Sender<()>>),
    /// Resolved; see `helper`.
    Done,
}

/// Shared with the start thread.
struct Inner {
    helper: OnceLock<Option<Arc<GpuHelper>>>,
    pending: Mutex<Pending>,
}

impl Inner {
    /// Publish the outcome, then wake every waiter.
    fn finish(&self, helper: Option<Arc<GpuHelper>>) {
        let _ = self.helper.set(helper);
        let waiters = match std::mem::replace(&mut *self.lock(), Pending::Done) {
            Pending::Starting(waiters) => waiters,
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
            }),
        }
    }

    /// A slot that runs `start` on the first [`Self::get_or_start`], then
    /// `on_start` with the helper when one started.
    pub fn lazy(start: Starter, on_start: OnStart) -> Self {
        Self {
            inner: Arc::new(Inner {
                helper: OnceLock::new(),
                pending: Mutex::new(Pending::Idle(start, Some(on_start))),
            }),
        }
    }

    /// The helper, if it has started. Never starts it.
    pub fn get(&self) -> Option<&Arc<GpuHelper>> {
        self.inner.helper.get().and_then(Option::as_ref)
    }

    /// Whether the slot still waits for its first `gpu-read`.
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
                Pending::Idle(start, on_start) => {
                    *pending = Pending::Starting(vec![sender]);
                    drop(pending);
                    self.spawn(start, on_start);
                }
                Pending::Starting(mut waiters) => {
                    waiters.push(sender);
                    *pending = Pending::Starting(waiters);
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
    fn spawn(&self, start: Starter, on_start: Option<OnStart>) {
        let inner = self.inner.clone();
        let spawned = std::thread::Builder::new()
            .name("racer-gpu-start".into())
            .spawn(move || {
                let helper = start().unwrap_or_else(|error| {
                    eprintln!("racer-dataplane: stage=gpu state=disabled reason={error}");
                    None
                });
                if let (Some(helper), Some(on_start)) = (&helper, on_start) {
                    on_start(helper);
                }
                inner.finish(helper);
            });
        if spawned.is_err() {
            eprintln!("racer-dataplane: stage=gpu state=disabled reason=spawn-failed");
            self.inner.finish(None);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
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
                Ok(helper)
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
    /// A start that fails or finds nothing is final: no retry.
    fn failure_is_final() {
        for result in [Ok(None), Err(crate::error::Error::Unavailable)] {
            let calls = Arc::new(AtomicUsize::new(0));
            let counted = calls.clone();
            let hooked = Arc::new(AtomicUsize::new(0));
            let hook = hooked.clone();
            let mut result = Some(result);
            let slot = GpuSlot::lazy(
                Box::new(move || {
                    counted.fetch_add(1, Ordering::SeqCst);
                    result.take().unwrap()
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
            assert!(!slot.standby());
        }
    }

    #[test]
    /// The start hook runs once, before any waiter sees the helper.
    fn on_start_runs_before_waiters_resume() {
        let helper = crate::gpu::testing::helper(&[None]);
        let hooked = Arc::new(AtomicUsize::new(0));
        let hook = hooked.clone();
        let started = helper.clone();
        let slot = GpuSlot::lazy(
            Box::new(move || Ok(Some(started))),
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
