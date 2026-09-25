//! Monotonic deadlines and bounded cancellation notification registrations.
use crate::{
    error::{Error, Result},
    model::identity::RequestId,
};
use std::{
    sync::{
        Arc, Mutex, Weak,
        atomic::{AtomicBool, Ordering},
    },
    task::Waker,
    time::Instant,
};
#[derive(Clone, Copy, Debug)]
pub struct Deadline(pub Instant);
#[derive(Clone)]
pub struct Cancellation {
    state: Arc<State>,
}
struct State {
    canceled: AtomicBool,
    waiters: Mutex<Vec<Waker>>,
    registrations: Mutex<Vec<Weak<futures::task::AtomicWaker>>>,
}
/// Operation-owned cancellation wake registration. Drop removes its live entry;
/// independent operations may safely register the same executor waker.
pub struct CancellationRegistration {
    cancellation: Cancellation,
    wake: Arc<futures::task::AtomicWaker>,
}
impl CancellationRegistration {
    pub fn register(&self, waker: &Waker) {
        self.wake.register(waker);
        if self.cancellation.is_cancelled() {
            self.wake.wake();
        }
    }
}
impl Drop for CancellationRegistration {
    fn drop(&mut self) {
        if let Ok(mut entries) = self.cancellation.state.registrations.lock() {
            entries.retain(|entry| {
                !std::ptr::eq(entry.as_ptr(), Arc::as_ptr(&self.wake)) && entry.strong_count() != 0
            });
        }
    }
}
impl Cancellation {
    pub fn new() -> Result<Self> {
        Ok(Self {
            state: Arc::new(State {
                canceled: AtomicBool::new(false),
                waiters: Mutex::new(Vec::new()),
                registrations: Mutex::new(Vec::new()),
            }),
        })
    }
    pub fn is_cancelled(&self) -> bool {
        self.state.canceled.load(Ordering::Acquire)
    }
    pub fn subscribe(&self) -> Result<CancellationRegistration> {
        let wake = Arc::new(futures::task::AtomicWaker::new());
        let mut entries = self
            .state
            .registrations
            .lock()
            .map_err(|_| Error::Unavailable)?;
        entries.retain(|entry| entry.strong_count() != 0);
        if entries.len() >= 1024 {
            return Err(Error::Overloaded);
        }
        entries.push(Arc::downgrade(&wake));
        Ok(CancellationRegistration {
            cancellation: self.clone(),
            wake,
        })
    }
    /// Register before checking cancellation. Identical executor wakers coalesce.
    pub fn register(&self, waker: &Waker) -> Result<()> {
        let mut waiters = self.state.waiters.lock().map_err(|_| Error::Unavailable)?;
        if self.is_cancelled() {
            drop(waiters);
            waker.wake_by_ref();
            return Ok(());
        }
        if !waiters.iter().any(|old| old.will_wake(waker)) {
            if waiters.len() >= 1024 {
                return Err(Error::Overloaded);
            }
            waiters.push(waker.clone());
        }
        Ok(())
    }
    /// Release an operation-specific registration on detach. Worker-global
    /// registrations should instead live for the entire request scope.
    pub fn unregister(&self, waker: &Waker) -> Result<()> {
        self.state
            .waiters
            .lock()
            .map_err(|_| Error::Unavailable)?
            .retain(|old| !old.will_wake(waker));
        Ok(())
    }
    pub fn cancel(&self) -> Result<()> {
        self.state.canceled.store(true, Ordering::Release);
        let waiters =
            std::mem::take(&mut *self.state.waiters.lock().map_err(|_| Error::Unavailable)?);
        for waker in waiters {
            waker.wake();
        }
        let registrations: Vec<_> = self
            .state
            .registrations
            .lock()
            .map_err(|_| Error::Unavailable)?
            .iter()
            .filter_map(Weak::upgrade)
            .collect();
        for registration in registrations {
            registration.wake();
        }
        Ok(())
    }
}
#[derive(Clone)]
pub struct RequestScope {
    pub request: RequestId,
    pub deadline: Deadline,
    pub cancellation: Cancellation,
}
impl RequestScope {
    pub fn new(request: RequestId, deadline: Instant) -> Result<Self> {
        Ok(Self {
            request,
            deadline: Deadline(deadline),
            cancellation: Cancellation::new()?,
        })
    }
    pub fn check(&self) -> Result<()> {
        if self.cancellation.is_cancelled() {
            Err(Error::Cancelled)
        } else if Instant::now() >= self.deadline.0 {
            Err(Error::DeadlineExceeded)
        } else {
            Ok(())
        }
    }
    pub fn cancel(&self) -> Result<()> {
        self.cancellation.cancel()
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use std::{sync::atomic::AtomicUsize, task::Wake, time::Duration};
    struct Count(AtomicUsize);
    impl Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }
    #[test]
    fn operation_registrations_reclaim_capacity_and_do_not_remove_shared_wakes() {
        let cancellation = Cancellation::new().unwrap();
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        for _ in 0..2048 {
            let registration = cancellation.subscribe().unwrap();
            registration.register(&waker);
        }
        assert!(cancellation.state.registrations.lock().unwrap().is_empty());
        let first = cancellation.subscribe().unwrap();
        let second = cancellation.subscribe().unwrap();
        first.register(&waker);
        second.register(&waker);
        drop(first);
        cancellation.cancel().unwrap();
        assert_eq!(count.0.load(Ordering::Relaxed), 1);
    }
    #[test]
    fn clones_preserve_deadline_and_wake_independent_waiters() {
        let scope =
            RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(2)).unwrap();
        let a = Arc::new(Count(AtomicUsize::new(0)));
        let b = Arc::new(Count(AtomicUsize::new(0)));
        scope
            .cancellation
            .register(&Waker::from(a.clone()))
            .unwrap();
        scope
            .cancellation
            .register(&Waker::from(b.clone()))
            .unwrap();
        let clone = scope.clone();
        assert_eq!(clone.deadline.0, scope.deadline.0);
        clone.cancel().unwrap();
        assert_eq!(scope.check(), Err(Error::Cancelled));
        assert_eq!(a.0.load(Ordering::Relaxed), 1);
        assert_eq!(b.0.load(Ordering::Relaxed), 1);
        assert_eq!(
            RequestScope::new(RequestId([1; 16]), Instant::now())
                .unwrap()
                .check(),
            Err(Error::DeadlineExceeded)
        );
    }
}
