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

    /// Convenience: one worker per supplied CPU id, with each
    /// worker's NUMA hint resolved from `/sys`.
    pub fn one_per_cpu<I: IntoIterator<Item = u32>>(cpus: I) -> Arc<Self> {
        let workers = cpus
            .into_iter()
            .map(|cpu| WorkerSpec {
                cpu,
                numa: numa_node_of_cpu(cpu),
            })
            .collect::<Vec<_>>();
        Self::new(workers)
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

    fn run_worker(&self, idx: WorkerIdx, f: Box<dyn FnOnce() + Send + 'static>) {
        pin_current(self.spec(idx)).expect("PinnedRuntime::run_worker: pin failed");
        f();
    }

    fn spawn_aux(
        &self,
        idx: WorkerIdx,
        name: &str,
        f: Box<dyn FnOnce() + Send + 'static>,
    ) -> JoinHandle {
        let spec = self.spec(idx);
        std::thread::Builder::new()
            .name(name.to_string())
            .spawn(move || {
                // A pin failure inside an aux thread shouldn't abort
                // the host process; the work still has to run.
                if let Err(e) = pin_current(spec) {
                    eprintln!(
                        "PinnedRuntime::spawn_aux: pin failed for cpu={} numa={:?}: {e}",
                        spec.cpu, spec.numa
                    );
                }
                f();
            })
            .expect("PinnedRuntime::spawn_aux: thread spawn failed")
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
    // SAFETY: cpu_set_t is a POD bitmap; we zero it and only call
    // libc helpers that take a valid &mut.
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

fn numa_node_of_cpu(cpu: u32) -> Option<u16> {
    let dir = format!("/sys/devices/system/cpu/cpu{cpu}");
    for entry in std::fs::read_dir(&dir).ok()?.flatten() {
        let name = entry.file_name();
        if let Some(rest) = name.to_string_lossy().strip_prefix("node") {
            if let Ok(n) = rest.parse::<u16>() {
                return Some(n);
            }
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    // CPU 0 exists on every Linux host the test suite runs against.
    const TEST_CPU: u32 = 0;

    #[test]
    fn run_worker_pins_to_target_cpu() {
        let rt = PinnedRuntime::new(vec![WorkerSpec::new(TEST_CPU, None)]);
        let pinned = Arc::new(Mutex::new(false));
        let p = pinned.clone();
        rt.run_worker(
            WorkerIdx(0),
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
        assert!(*pinned.lock().unwrap());
    }

    #[test]
    fn spawn_aux_runs_and_joins() {
        let rt = PinnedRuntime::new(vec![WorkerSpec::new(TEST_CPU, Some(7))]);
        assert_eq!(rt.worker_count(), 1);
        assert_eq!(rt.numa_of(WorkerIdx(0)), Some(7));
        let observed = Arc::new(Mutex::new(false));
        let o = observed.clone();
        // numa=Some(7) may fail on hosts without that node; pin
        // with numa=None for the aux side to avoid that.
        let rt2 = PinnedRuntime::new(vec![WorkerSpec::new(TEST_CPU, None)]);
        let h = rt2.spawn_aux(
            WorkerIdx(0),
            "pinned-aux",
            Box::new(move || *o.lock().unwrap() = true),
        );
        h.join().expect("aux thread");
        assert!(*observed.lock().unwrap());
    }
}
