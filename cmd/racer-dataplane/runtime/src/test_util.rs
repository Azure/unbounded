//! Optional shared test instrumentation; never enabled by production dependencies.
use std::sync::{
    Arc,
    atomic::{AtomicUsize, Ordering},
};

#[derive(Default)]
pub struct WakeCounter(AtomicUsize);

impl WakeCounter {
    pub fn count(&self) -> usize {
        self.0.load(Ordering::SeqCst)
    }
}

impl std::task::Wake for WakeCounter {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::SeqCst);
    }
}

#[test]
fn counts_owned_and_borrowed_wakes() {
    let counter = Arc::new(WakeCounter::default());
    let waker = std::task::Waker::from(counter.clone());
    waker.wake_by_ref();
    waker.wake();
    assert_eq!(counter.count(), 2);
}
