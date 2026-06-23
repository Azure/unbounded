// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::sync::Arc;

#[cfg(target_os = "linux")]
mod pinned;

mod executor;
mod shard;

#[cfg(target_os = "linux")]
mod context;

#[cfg(target_os = "linux")]
pub use pinned::{PinnedRuntime, WorkerSpec, set_preferred_node};

#[cfg(target_os = "linux")]
pub use context::ShardContext;

pub use executor::{block_on_cooperative, noop_waker, park_block_on_until, thread_waker};
pub use shard::ShardLoop;

/// Universal identifier for a worker slot. The bufferpool runs one
/// `Pool` per worker, the fabric layer constructs one `Fabric` per worker, and
/// the future io_uring layer registers one submission ring per
/// worker. All three pin against the same `WorkerIdx`.
#[derive(Copy, Clone, Eq, PartialEq, Hash, Debug)]
pub struct WorkerIdx(pub u16);

/// Handle to a thread spawned via [`Threading::spawn_pinned`].
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

    /// Spawn a new named OS thread for worker `idx`. Inside the new
    /// thread the runtime pins to the worker's CPU and applies its
    /// NUMA mempolicy *first*, then runs `f` to completion. The
    /// returned [`JoinHandle`] joins that thread.
    fn spawn_pinned(
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

    fn spawn_pinned(
        &self,
        idx: WorkerIdx,
        name: &str,
        f: Box<dyn FnOnce() + Send + 'static>,
    ) -> JoinHandle {
        debug_assert!((idx.0 as usize) < self.workers);
        std::thread::Builder::new()
            .name(name.to_string())
            .spawn(f)
            .expect("DefaultRuntime::spawn_pinned: thread spawn failed")
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
        let h = rt.spawn_pinned(
            WorkerIdx(0),
            "default-pinned-0",
            Box::new(move || *o.lock().unwrap() = 1),
        );
        h.join().expect("pinned thread");
        assert_eq!(*observed.lock().unwrap(), 1);

        let o = observed.clone();
        let h = rt.spawn_pinned(
            WorkerIdx(1),
            "default-pinned-1",
            Box::new(move || *o.lock().unwrap() = 2),
        );
        h.join().expect("pinned thread");
        assert_eq!(*observed.lock().unwrap(), 2);
    }
}
