// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::sync::Arc;

use super::{JoinHandle, Threading, WorkerIdx};

/// Pin spec for one worker slot. The embedder enumerates topology;
/// `PinnedRuntime` only consumes the result.
#[derive(Clone, Copy, Debug)]
pub struct WorkerSpec {
    /// Logical CPU id to pin the worker thread to.
    pub cpu: u32,
    /// NUMA node to bind memory allocations to. `None` leaves the
    /// kernel default policy in place.
    pub numa: Option<u16>,
}

impl WorkerSpec {
    pub const fn new(cpu: u32, numa: Option<u16>) -> Self {
        Self { cpu, numa }
    }
}

/// Pinned, NUMA-aware `Threading` implementation.
pub struct PinnedRuntime {
    workers: Vec<WorkerSpec>,
}

impl PinnedRuntime {
    pub fn new(workers: Vec<WorkerSpec>) -> Arc<Self> {
        assert!(
            !workers.is_empty(),
            "PinnedRuntime requires at least one worker"
        );
        Arc::new(Self { workers })
    }

    /// Build a runtime from a topology [`Plan`](crate::topology::Plan),
    /// keeping only workers for which `filter` returns true, in the
    /// plan's iteration order. Each retained worker maps to a
    /// [`WorkerSpec`] over its CPU and NUMA node, so `WorkerIdx(i)`
    /// addresses the i-th retained worker. Because the order is
    /// preserved, callers that independently filter `plan.workers`
    /// with the *same* predicate get a matching index space.
    pub fn from_plan(
        plan: &crate::topology::Plan,
        filter: impl Fn(&crate::topology::Worker) -> bool,
    ) -> Arc<Self> {
        let workers = plan
            .workers
            .iter()
            .filter(|w| filter(w))
            .map(|w| WorkerSpec::new(w.cpu, w.numa))
            .collect::<Vec<_>>();
        assert!(
            !workers.is_empty(),
            "PinnedRuntime::from_plan: no workers matched the filter"
        );
        Self::new(workers)
    }

    /// Spawn a named OS thread and, when `spec` is `Some`, pin it
    /// (CPU affinity + NUMA mempolicy) inside the new thread before
    /// running `f`. `None` spawns an unpinned thread. This is the one
    /// place the runtime turns a placement into a live thread.
    pub fn spawn_placed(
        &self,
        spec: Option<WorkerSpec>,
        name: &str,
        f: Box<dyn FnOnce() + Send + 'static>,
    ) -> JoinHandle {
        std::thread::Builder::new()
            .name(name.to_string())
            .spawn(move || {
                // Pin (affinity + NUMA mempolicy) inside the new thread
                // BEFORE running `f`, so any allocations `f` makes land
                // on the worker's NUMA node. A pin failure here must not
                // abort the host process; the work still has to run.
                if let Some(spec) = spec {
                    if let Err(e) = pin_current(spec) {
                        eprintln!(
                            "PinnedRuntime::spawn_placed: pin failed for cpu={} numa={:?}: {e}",
                            spec.cpu, spec.numa
                        );
                    }
                }
                f();
            })
            .expect("PinnedRuntime::spawn_placed: thread spawn failed")
    }

    fn spec(&self, idx: WorkerIdx) -> WorkerSpec {
        let i = idx.0 as usize;
        assert!(i < self.workers.len(), "WorkerIdx out of range");
        self.workers[i]
    }
}

impl Threading for PinnedRuntime {
    fn worker_count(&self) -> usize {
        self.workers.len()
    }

    fn numa_of(&self, idx: WorkerIdx) -> Option<u16> {
        self.spec(idx).numa
    }

    fn spawn_pinned(
        &self,
        idx: WorkerIdx,
        name: &str,
        f: Box<dyn FnOnce() + Send + 'static>,
    ) -> JoinHandle {
        self.spawn_placed(Some(self.spec(idx)), name, f)
    }
}

fn pin_current(spec: WorkerSpec) -> std::io::Result<()> {
    set_affinity(spec.cpu)?;
    if let Some(node) = spec.numa {
        set_preferred_node(node)?;
    }
    Ok(())
}

fn set_affinity(cpu: u32) -> std::io::Result<()> {
    // `cpu_set_t` is a fixed-size bitmap holding `CPU_SETSIZE` bits.
    // `CPU_SET` with an index past that capacity writes past the end
    // of the on-stack set (undefined behavior / stack corruption), so
    // bound-check first. Hosts with more than `CPU_SETSIZE` logical
    // CPUs exist and sysfs CPU ids can exceed this limit.
    let capacity = std::mem::size_of::<libc::cpu_set_t>() * 8;
    if cpu as usize >= capacity {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            format!("cpu {cpu} exceeds cpu_set_t capacity of {capacity} bits"),
        ));
    }
    // SAFETY: cpu_set_t is a POD bitmap; we zero it and only call libc
    // helpers that take a valid &mut. The bound check above guarantees
    // `cpu` is in range for `CPU_SET`.
    unsafe {
        let mut set: libc::cpu_set_t = std::mem::zeroed();
        libc::CPU_ZERO(&mut set);
        libc::CPU_SET(cpu as usize, &mut set);
        let rc =
            libc::sched_setaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &set as *const _);
        if rc != 0 {
            return Err(std::io::Error::last_os_error());
        }
    }
    Ok(())
}

fn set_preferred_node(node: u16) -> std::io::Result<()> {
    // MPOL_PREFERRED: allocate from `node` if possible, fall back
    // otherwise. Strict binding is too aggressive for a cache that
    // can spill onto remote memory when local is full.
    const MPOL_PREFERRED: libc::c_int = 1;
    let bits_per_word = std::mem::size_of::<libc::c_ulong>() * 8;
    let bit = node as usize;
    let words = bit / bits_per_word + 1;
    let mut mask = vec![0 as libc::c_ulong; words];
    mask[bit / bits_per_word] |= (1 as libc::c_ulong) << (bit % bits_per_word);
    let maxnode = (words * bits_per_word) as libc::c_ulong;
    // SAFETY: mask is a valid, properly-sized bitmask for the syscall.
    let rc = unsafe {
        libc::syscall(
            libc::SYS_set_mempolicy,
            MPOL_PREFERRED as libc::c_long,
            mask.as_ptr() as libc::c_long,
            maxnode as libc::c_long,
        )
    };
    if rc != 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    // CPU 0 exists on every Linux host the test suite runs against.
    const TEST_CPU: u32 = 0;

    #[test]
    fn spawn_pinned_pins_to_target_cpu() {
        let rt = PinnedRuntime::new(vec![WorkerSpec::new(TEST_CPU, None)]);
        let pinned = Arc::new(Mutex::new(false));
        let p = pinned.clone();
        let h = rt.spawn_pinned(
            WorkerIdx(0),
            "pinned-cpu-check",
            Box::new(move || {
                let mut got: libc::cpu_set_t = unsafe { std::mem::zeroed() };
                let rc = unsafe {
                    libc::sched_getaffinity(0, std::mem::size_of::<libc::cpu_set_t>(), &mut got)
                };
                assert_eq!(rc, 0);
                let only_test = unsafe { libc::CPU_ISSET(TEST_CPU as usize, &got) }
                    && (0..libc::CPU_SETSIZE as usize)
                        .filter(|c| *c != TEST_CPU as usize)
                        .all(|c| !unsafe { libc::CPU_ISSET(c, &got) });
                *p.lock().unwrap() = only_test;
            }),
        );
        h.join().expect("pinned thread");
        assert!(*pinned.lock().unwrap());
    }

    #[test]
    fn set_affinity_rejects_out_of_range_cpu() {
        // A cpu id at or beyond the cpu_set_t bit capacity must be
        // rejected without invoking CPU_SET out of bounds.
        let capacity = std::mem::size_of::<libc::cpu_set_t>() * 8;
        let err = set_affinity(capacity as u32).expect_err("at-capacity cpu must be rejected");
        assert_eq!(err.kind(), std::io::ErrorKind::InvalidInput);
        let err = set_affinity(u32::MAX).expect_err("u32::MAX cpu must be rejected");
        assert_eq!(err.kind(), std::io::ErrorKind::InvalidInput);
    }

    #[test]
    fn spawn_pinned_runs_and_joins() {
        let rt = PinnedRuntime::new(vec![WorkerSpec::new(TEST_CPU, Some(7))]);
        assert_eq!(rt.worker_count(), 1);
        assert_eq!(rt.numa_of(WorkerIdx(0)), Some(7));
        let observed = Arc::new(Mutex::new(false));
        let o = observed.clone();
        // numa=Some(7) may fail on hosts without that node; pin
        // with numa=None for this side to avoid that.
        let rt2 = PinnedRuntime::new(vec![WorkerSpec::new(TEST_CPU, None)]);
        let h = rt2.spawn_pinned(
            WorkerIdx(0),
            "pinned-run",
            Box::new(move || *o.lock().unwrap() = true),
        );
        h.join().expect("pinned thread");
        assert!(*observed.lock().unwrap());
    }
}
