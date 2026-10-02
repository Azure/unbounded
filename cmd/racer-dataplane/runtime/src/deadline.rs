//! Monotonic deadlines and bounded cancellation notification registrations.
use crate::{Error, Result};
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
                registrations: Mutex::new(Vec::new()),
            }),
        })
    }

    pub fn is_cancelled(&self) -> bool {
        self.state.canceled.load(Ordering::Acquire)
    }

    /// Subscribe for one operation's lifetime, even when several operations share
    /// an executor waker. Dropping the subscription reclaims its capacity and wake.
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

    pub fn cancel(&self) -> Result<()> {
        self.state.canceled.store(true, Ordering::Release);
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::{sync::atomic::AtomicUsize, task::Wake};

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
    fn dropping_subscription_releases_executor_resources_before_scope_ends() {
        let cancellation = Cancellation::new().unwrap();
        let executor = Arc::new(Count(AtomicUsize::new(0)));
        let weak = Arc::downgrade(&executor);
        let registration = cancellation.subscribe().unwrap();
        registration.register(&Waker::from(executor));
        assert!(weak.upgrade().is_some());
        drop(registration);
        assert!(weak.upgrade().is_none());
        assert!(!cancellation.is_cancelled());

        // A worker that subscribes after cancellation must still be notified.
        cancellation.cancel().unwrap();
        let executor = Arc::new(Count(AtomicUsize::new(0)));
        let registration = cancellation.subscribe().unwrap();
        registration.register(&Waker::from(executor.clone()));
        assert_eq!(executor.0.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn live_registration_limit_is_reclaimed_after_drop() {
        let cancellation = Cancellation::new().unwrap();
        let mut registrations: Vec<_> = (0..1024)
            .map(|_| cancellation.subscribe().unwrap())
            .collect();
        assert!(matches!(cancellation.subscribe(), Err(Error::Overloaded)));
        registrations.pop();
        assert!(cancellation.subscribe().is_ok());
    }
}
