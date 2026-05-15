// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::sync::Arc;

#[cfg(target_os = "linux")]
mod pinned;

#[cfg(target_os = "linux")]
pub use pinned::{PinnedRuntime, WorkerSpec};

/// Universal identifier for a worker slot. The bufferpool runs one
/// `Pool` per worker, Mercury constructs one `Class` per worker, and
/// the future io_uring layer registers one submission ring per
/// worker. All three pin against the same `WorkerIdx`.
#[derive(Copy, Clone, Eq, PartialEq, Hash, Debug)]
pub struct WorkerIdx(pub u16);

/// Locality hint for the future io_uring submission API
/// (PLAN.md Phase 3.2). Declared here, not yet consumed.
#[derive(Copy, Clone, Eq, PartialEq, Debug)]
pub enum NumaHint {
    Any,
    Worker(WorkerIdx),
    Numa(u16),
}

/// Handle to an auxiliary thread spawned via [`Threading::spawn_aux`].
/// A type alias because no runtime impl needs to hide the underlying
/// thread; callers just want to join.
pub type JoinHandle = std::thread::JoinHandle<()>;

/// Worker-placement runtime trait. See PLAN.md Phase 3.1.
pub trait Threading: Send + Sync + 'static {
    /// Number of worker slots configured. `WorkerIdx(i)` is valid
    /// for `i < worker_count()`.
    fn worker_count(&self) -> usize;

    /// NUMA node hosting `idx`, if the runtime is NUMA-aware.
    fn numa_of(&self, idx: WorkerIdx) -> Option<u16>;

    /// Pin the *current* thread to worker `idx` and run `f` to
    /// completion. Blocks until `f` returns.
    fn run_worker(&self, idx: WorkerIdx, f: Box<dyn FnOnce() + Send + 'static>);

    /// Spawn an auxiliary OS thread pinned to the same NUMA node as
    /// worker `idx`.
    fn spawn_aux(
        &self,
        idx: WorkerIdx,
        name: &str,
        f: Box<dyn FnOnce() + Send + 'static>,
    ) -> JoinHandle;
}

/// Non-pinning `Threading` impl. Used in tests and on non-Linux
/// targets where the pinning syscalls don't exist.
pub struct DefaultRuntime {
    workers: usize,
}

impl DefaultRuntime {
    pub fn new(workers: usize) -> Arc<Self> {
        assert!(workers > 0, "DefaultRuntime requires at least one worker");
        Arc::new(Self { workers })
    }
}

impl Threading for DefaultRuntime {
    fn worker_count(&self) -> usize {
        self.workers
    }

    fn numa_of(&self, idx: WorkerIdx) -> Option<u16> {
        debug_assert!((idx.0 as usize) < self.workers);
        None
    }

    fn run_worker(&self, idx: WorkerIdx, f: Box<dyn FnOnce() + Send + 'static>) {
        debug_assert!((idx.0 as usize) < self.workers);
        f();
    }

    fn spawn_aux(
        &self,
        idx: WorkerIdx,
        name: &str,
        f: Box<dyn FnOnce() + Send + 'static>,
    ) -> JoinHandle {
        debug_assert!((idx.0 as usize) < self.workers);
        std::thread::Builder::new()
            .name(name.to_string())
            .spawn(f)
            .expect("DefaultRuntime::spawn_aux: thread spawn failed")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    #[test]
    fn default_runtime_runs_and_spawns() {
        let rt = DefaultRuntime::new(2);
        assert_eq!(rt.worker_count(), 2);
        assert!(rt.numa_of(WorkerIdx(0)).is_none());

        let observed = Arc::new(Mutex::new(0u32));
        let o = observed.clone();
        rt.run_worker(WorkerIdx(0), Box::new(move || *o.lock().unwrap() = 1));
        assert_eq!(*observed.lock().unwrap(), 1);

        let o = observed.clone();
        let h = rt.spawn_aux(
            WorkerIdx(1),
            "default-aux",
            Box::new(move || *o.lock().unwrap() = 2),
        );
        h.join().expect("aux thread");
        assert_eq!(*observed.lock().unwrap(), 2);
    }
}
