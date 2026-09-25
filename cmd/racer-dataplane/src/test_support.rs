//! Deterministic test-only seams; no fake implementation is linked into production.
pub mod clock;
pub mod cluster;
pub mod disk;
pub mod io;
pub mod origin;
pub mod rdma;

/// Single-poll helpers deliberately do not spin an executor or sleep on host time.
pub fn poll_once<T>(
    future: std::pin::Pin<&mut impl std::future::Future<Output = T>>,
) -> std::task::Poll<T> {
    future.poll(&mut std::task::Context::from_waker(std::task::Waker::noop()))
}

#[derive(Default)]
pub struct WakeCounter(std::sync::atomic::AtomicUsize);

impl WakeCounter {
    pub fn count(&self) -> usize {
        self.0.load(std::sync::atomic::Ordering::SeqCst)
    }
}

impl std::task::Wake for WakeCounter {
    fn wake(self: std::sync::Arc<Self>) {
        self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
    }
}
