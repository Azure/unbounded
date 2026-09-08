// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Progress threads for the connection-oriented `FI_EP_MSG` transport.
//!
//! Each connection ([`super::cm::Connection`]) owns its own completion
//! queue. A [`ProgressGroup`] owns a small pool of pinned OS threads
//! (one per NUMA node in the common deployment) that round-robin poll
//! every connection CQ registered with them. Threads pull `fi_cq_read`
//! batches in [`FI_CQ_FORMAT_DATA`] form, drain errors via
//! `fi_cq_readerr`, and resolve the `CompletionSlot` Box libfabric
//! handed back as the op_context. Shutdown is cooperative via an
//! `AtomicBool` checked each iteration.
//!
//! Registration is dynamic: connections come and go, so each thread
//! reads its CQ set from a shared [`CqRegistry`] guarded by a mutex and
//! snapshots it once per outer loop. A connection's CQ MUST be
//! unregistered (via [`ProgressGroup::unregister`]) before the
//! connection is dropped and its CQ closed, otherwise a progress thread
//! could poll a freed handle.
//!
//! [`FI_CQ_FORMAT_DATA`]: super::ffi::FI_CQ_FORMAT_DATA

use std::ffi::c_void;
use std::mem::MaybeUninit;
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::Duration;

use super::completion::{CompletionInfo, CompletionSlot};
use super::config::FabricConfig;
use super::error::{FabricError, Result};
use super::ffi;

const CQ_BATCH: usize = 16;

/// `Send` wrapper around a raw CQ pointer so it can live in the shared
/// registry and be moved into a progress thread without making
/// `*mut fid_cq` globally `Send`.
#[derive(Copy, Clone, PartialEq, Eq)]
pub(crate) struct CqPtr(pub(crate) *mut ffi::fid_cq);
// SAFETY: the connection that owns the CQ guarantees it outlives its
// registration (it is unregistered before close); libfabric CQs are
// internally synchronized so concurrent readers are sound.
unsafe impl Send for CqPtr {}

/// Shared, mutable set of CQs one progress thread polls. Cloned (by
/// `Arc`) between the thread and the [`ProgressGroup`] that registers
/// into it.
struct CqRegistry {
    cqs: Mutex<Vec<CqPtr>>,
}

impl CqRegistry {
    fn new() -> Self {
        CqRegistry {
            cqs: Mutex::new(Vec::new()),
        }
    }

    fn add(&self, cq: CqPtr) {
        let mut g = self.cqs.lock().unwrap();
        if !g.contains(&cq) {
            g.push(cq);
        }
    }

    /// Remove `cq`; returns true if it was present.
    fn remove(&self, cq: CqPtr) -> bool {
        let mut g = self.cqs.lock().unwrap();
        if let Some(i) = g.iter().position(|c| *c == cq) {
            g.swap_remove(i);
            true
        } else {
            false
        }
    }

    fn snapshot(&self) -> Vec<CqPtr> {
        self.cqs.lock().unwrap().clone()
    }
}

/// One pinned progress thread polling a dynamic set of connection CQs.
struct ProgressThread {
    numa: u16,
    registry: Arc<CqRegistry>,
    shutdown: Arc<AtomicBool>,
    transient_errors: Arc<AtomicU64>,
    /// Monotonic counter bumped once at the top of every poll-loop
    /// iteration. [`ProgressGroup::unregister`] reads it to wait out an
    /// in-flight poll cycle that may still hold a stale snapshot of the
    /// CQ being removed, so callers can close the CQ without UAF.
    epoch: Arc<AtomicU64>,
    /// Behind a `Mutex` so [`ProgressGroup::stop`] can signal and join
    /// through a shared `&self` (the group is held in an `Arc`), while
    /// `Drop` remains a safe idempotent fallback.
    handle: Mutex<Option<JoinHandle<()>>>,
}

impl ProgressThread {
    /// Signal shutdown and join the thread. Idempotent: a second call
    /// finds the handle already taken and returns immediately.
    fn stop(&self) {
        self.shutdown.store(true, Ordering::Release);
        if let Some(h) = self.handle.lock().unwrap().take() {
            let _ = h.join();
        }
    }
}

impl Drop for ProgressThread {
    fn drop(&mut self) {
        self.stop();
    }
}

/// A pool of progress threads serving all connection CQs of a fabric.
/// Registration round-robins across threads, preferring a thread whose
/// NUMA label matches the connection's locality when one exists.
pub(crate) struct ProgressGroup {
    threads: Vec<ProgressThread>,
    next: AtomicUsize,
}

// SAFETY: every field is `Send`/`Sync`; the raw CQ pointers live behind
// the `CqRegistry` mutex and uphold the unregister-before-close rule.
unsafe impl Send for ProgressGroup {}
unsafe impl Sync for ProgressGroup {}

impl ProgressGroup {
    /// Spawn the progress pool, distributing threads over the fabric
    /// unit's reserved workers.
    pub(crate) fn new(cfg: &FabricConfig) -> Result<Self> {
        let count = (cfg.progress_threads as usize).max(1);
        let mut threads = Vec::with_capacity(count);

        for i in 0..count {
            let worker_idx = cfg.worker_for_thread(i);
            let numa = cfg.runtime.numa_of(worker_idx).or(cfg.numa).unwrap_or(0);
            let registry = Arc::new(CqRegistry::new());
            let shutdown = Arc::new(AtomicBool::new(false));
            let transient_errors = Arc::new(AtomicU64::new(0));
            let epoch = Arc::new(AtomicU64::new(0));

            let loop_registry = Arc::clone(&registry);
            let loop_shutdown = Arc::clone(&shutdown);
            let loop_errors = Arc::clone(&transient_errors);
            let loop_epoch = Arc::clone(&epoch);
            let poll_us = cfg.progress_poll_us;
            let name = format!("fabric-progress-{numa}-{i}");

            let handle = cfg.runtime.spawn_pinned(
                worker_idx,
                &name,
                Box::new(move || {
                    progress_loop(
                        loop_registry,
                        loop_shutdown,
                        loop_errors,
                        loop_epoch,
                        poll_us,
                    );
                }),
            );

            threads.push(ProgressThread {
                numa,
                registry,
                shutdown,
                transient_errors,
                epoch,
                handle: Mutex::new(Some(handle)),
            });
        }

        Ok(ProgressGroup {
            threads,
            next: AtomicUsize::new(0),
        })
    }

    /// Register a connection CQ for polling. `numa` is the connection's
    /// locality: if a thread carries the same NUMA label it is
    /// preferred, otherwise registration round-robins. The CQ must be
    /// unregistered before it is closed.
    pub(crate) fn register(&self, numa: Option<u16>, cq: *mut ffi::fid_cq) {
        if cq.is_null() || self.threads.is_empty() {
            return;
        }
        let idx = self.pick_thread(numa);
        self.threads[idx].registry.add(CqPtr(cq));
    }

    /// Stop polling `cq`. Call this before the owning connection drops
    /// and closes the CQ. Searches every thread since a CQ is registered
    /// with exactly one. After removing the CQ from a thread's registry
    /// this waits out any poll cycle that was in flight at removal time:
    /// `progress_loop` snapshots the registry and then dereferences the
    /// raw CQ pointers with no lock held, so a thread may still hold a
    /// stale snapshot containing `cq`. Waiting for the thread's epoch to
    /// advance by two guarantees the in-flight iteration (and thus any
    /// pending deref of `cq`) has completed before the caller closes it.
    pub(crate) fn unregister(&self, cq: *mut ffi::fid_cq) {
        if cq.is_null() {
            return;
        }
        let target = CqPtr(cq);
        for t in &self.threads {
            if t.registry.remove(target) {
                let start = t.epoch.load(Ordering::Acquire);
                while t.epoch.load(Ordering::Acquire).wrapping_sub(start) < 2 {
                    if t.shutdown.load(Ordering::Acquire) {
                        break;
                    }
                    std::thread::yield_now();
                }
                return;
            }
        }
    }

    /// Signal every progress thread to stop and join it. Idempotent and
    /// safe to call through a shared `Arc<ProgressGroup>`; the fabric
    /// teardown calls this after all CQs are unregistered and before the
    /// domain is closed, so no thread polls a closing CQ. `Drop` of the
    /// owned threads is a fallback for paths that never call `stop`.
    pub(crate) fn stop(&self) {
        for t in &self.threads {
            t.stop();
        }
    }

    fn pick_thread(&self, numa: Option<u16>) -> usize {
        let start = self.next.fetch_add(1, Ordering::Relaxed) % self.threads.len();
        if let Some(n) = numa {
            for offset in 0..self.threads.len() {
                let idx = (start + offset) % self.threads.len();
                if self.threads[idx].numa == n {
                    return idx;
                }
            }
        }
        start
    }

    /// Total transient (non-EAGAIN, non-EAVAIL) error count across all
    /// threads. Visible to tests / metrics.
    #[cfg(test)]
    pub(crate) fn transient_error_count(&self) -> u64 {
        self.threads
            .iter()
            .map(|t| t.transient_errors.load(Ordering::Relaxed))
            .sum()
    }
}

fn progress_loop(
    registry: Arc<CqRegistry>,
    shutdown: Arc<AtomicBool>,
    errors: Arc<AtomicU64>,
    epoch: Arc<AtomicU64>,
    poll_us: u32,
) {
    let mut batch: [MaybeUninit<ffi::fi_cq_data_entry>; CQ_BATCH] =
        unsafe { MaybeUninit::uninit().assume_init() };
    let eagain = unsafe { ffi::ub_fi_eagain() } as isize;
    let eavail = unsafe { ffi::ub_fi_eavail() } as isize;
    let idle = Duration::from_micros(poll_us as u64);

    while !shutdown.load(Ordering::Acquire) {
        epoch.fetch_add(1, Ordering::Release);
        let cqs = registry.snapshot();
        if cqs.is_empty() {
            std::thread::sleep(idle);
            continue;
        }

        let mut any = false;
        for cq in &cqs {
            // SAFETY: the CQ is registered (so not yet closed) and sized
            // for `CQ_BATCH` `fi_cq_data_entry` slots.
            let n =
                unsafe { ffi::ub_fi_cq_read(cq.0, batch.as_mut_ptr() as *mut c_void, CQ_BATCH) };

            if n > 0 {
                any = true;
                for slot in batch.iter().take(n as usize) {
                    // SAFETY: libfabric initialized at least `n` entries.
                    let entry = unsafe { slot.assume_init_ref() };
                    deliver_success(entry);
                }
                continue;
            }

            if n == 0 || n == -eagain {
                continue;
            }

            if n == -eavail {
                any = true;
                drain_errors(cq.0, &errors);
                continue;
            }

            errors.fetch_add(1, Ordering::Relaxed);
        }

        if !any {
            std::thread::sleep(idle);
        }
    }
}

fn deliver_success(entry: &ffi::fi_cq_data_entry) {
    let ctx = entry.op_context;
    if ctx.is_null() {
        return;
    }
    // SAFETY: `op_context` is the pointer handed in via
    // `CompletionSlot::into_raw` at submission; libfabric returns it
    // untouched on completion. Under MSG there is no tag or source
    // address, so those fields are zero.
    let mut slot = unsafe { CompletionSlot::from_raw(ctx) };
    slot.complete(Ok(CompletionInfo {
        flags: entry.flags,
        bytes: entry.len,
        tag: 0,
        src_addr: 0,
        op_context: ctx as usize,
        data: entry.data,
    }));
}

fn drain_errors(cq: *mut ffi::fid_cq, errors: &AtomicU64) {
    loop {
        let mut err_entry: ffi::fi_cq_err_entry = unsafe { std::mem::zeroed() };
        // SAFETY: cq is valid; err_entry is a fresh out-param sized to
        // libfabric's stable ABI layout.
        let rc = unsafe { ffi::ub_fi_cq_readerr(cq, &mut err_entry, 0) };
        if rc <= 0 {
            if rc < 0 {
                errors.fetch_add(1, Ordering::Relaxed);
            }
            break;
        }
        let ctx = err_entry.op_context;
        if !ctx.is_null() {
            // SAFETY: same provenance contract as `deliver_success`.
            let mut slot = unsafe { CompletionSlot::from_raw(ctx) };
            slot.complete(Err(FabricError::Cq {
                prov_errno: err_entry.prov_errno,
                err: err_entry.err,
            }));
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::DefaultRuntime;

    fn test_cfg(progress_threads: u8) -> FabricConfig {
        let mut cfg = crate::fabric::config::defaults_for(
            "tcp-loopback",
            DefaultRuntime::new(1),
            crate::runtime::WorkerIdx(0),
        );
        cfg.progress_threads = progress_threads;
        cfg.progress_poll_us = 1;
        cfg.numa = Some(0);
        cfg
    }

    #[test]
    fn registry_add_is_idempotent_and_remove_reports_presence() {
        let r = CqRegistry::new();
        let a = CqPtr(1 as *mut ffi::fid_cq);
        let b = CqPtr(2 as *mut ffi::fid_cq);
        r.add(a);
        r.add(a);
        r.add(b);
        assert_eq!(r.snapshot().len(), 2);
        assert!(r.remove(a));
        assert!(!r.remove(a));
        assert_eq!(r.snapshot().len(), 1);
    }

    #[test]
    fn group_spawns_and_shuts_down_cleanly() {
        let cfg = test_cfg(2);
        let group = ProgressGroup::new(&cfg).expect("group");
        assert_eq!(group.threads.len(), 2);
        // No CQs registered: threads idle on the empty-registry path.
        drop(group);
    }

    #[test]
    fn register_unregister_routes_to_one_thread() {
        let cfg = test_cfg(2);
        let group = ProgressGroup::new(&cfg).expect("group");
        // Quiesce the poll threads before installing a sentinel CQ
        // pointer: this test exercises only the registry routing
        // bookkeeping, and a live progress thread would dereference the
        // bogus pointer. Production always registers real, pollable CQs.
        group.stop();
        let cq = 0x1234 as *mut ffi::fid_cq;
        group.register(Some(0), cq);
        let total: usize = group
            .threads
            .iter()
            .map(|t| t.registry.snapshot().len())
            .sum();
        assert_eq!(total, 1);
        group.unregister(cq);
        let total: usize = group
            .threads
            .iter()
            .map(|t| t.registry.snapshot().len())
            .sum();
        assert_eq!(total, 0);
        drop(group);
    }

    #[test]
    fn matching_numa_registrations_round_robin() {
        let cfg = test_cfg(2);
        let group = ProgressGroup::new(&cfg).expect("group");
        group.stop();

        let a = 0x1234 as *mut ffi::fid_cq;
        let b = 0x5678 as *mut ffi::fid_cq;
        group.register(Some(0), a);
        group.register(Some(0), b);

        let first = group.threads[0].registry.snapshot();
        let second = group.threads[1].registry.snapshot();
        assert!(first.len() == 1 && first[0] == CqPtr(a));
        assert!(second.len() == 1 && second[0] == CqPtr(b));
    }
}
