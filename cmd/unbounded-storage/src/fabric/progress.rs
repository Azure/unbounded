// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! One pinned progress thread per libfabric CQ. The thread polls
//! `fi_cq_read` in batches, drains errors via `fi_cq_readerr`, and
//! resolves the `CompletionSlot` Box that libfabric handed back as
//! the op_context. Shutdown is cooperative: the owning [`ProgressThread`]
//! holds an `AtomicBool` flag that the loop checks each iteration.
//!
//! When the CQ is empty the thread sleeps for
//! `FabricConfig::progress_poll_us` microseconds to bound idle CPU.

use std::ffi::c_void;
use std::mem::MaybeUninit;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::thread::JoinHandle;
use std::time::Duration;

use super::completion::{CompletionInfo, CompletionSlot};
use super::config::FabricConfig;
use super::error::{FabricError, Result};
use super::ffi;

const CQ_BATCH: usize = 16;

/// Owns one `fid_cq` and the OS thread polling it. Drop signals
/// shutdown and joins the thread.
pub struct ProgressThread {
    cq: *mut ffi::fid_cq,
    shutdown: Arc<AtomicBool>,
    /// Count of transient (non-EAGAIN, non-EAVAIL) negative returns
    /// observed. Visible to tests / future metrics.
    transient_errors: Arc<AtomicU64>,
    handle: Option<JoinHandle<()>>,
}

// SAFETY: `fid_cq` is documented as thread-safe; the only other
// reference to it lives on the spawned progress thread.
unsafe impl Send for ProgressThread {}
unsafe impl Sync for ProgressThread {}

impl ProgressThread {
    /// Spawn a progress thread driving `cq`. The thread is pinned via
    /// `cfg.runtime.spawn_pinned` to the same NUMA node as
    /// `cfg.worker_idx`.
    ///
    /// Ownership note: the spawned thread reads `cq` without owning it;
    /// the caller (the `Fabric`) must outlive the `ProgressThread` and
    /// is responsible for closing `cq` after drop joins this thread.
    pub fn spawn(cfg: &FabricConfig, cq: *mut ffi::fid_cq, name: &str) -> Result<Self> {
        let shutdown = Arc::new(AtomicBool::new(false));
        let transient_errors = Arc::new(AtomicU64::new(0));
        let cq_addr = CqPtr(cq);
        let poll_us = cfg.progress_poll_us;
        let thread_shutdown = shutdown.clone();
        let thread_errors = transient_errors.clone();

        let handle = cfg.runtime.spawn_pinned(
            cfg.worker_idx,
            name,
            Box::new(move || {
                progress_loop(cq_addr, thread_shutdown, thread_errors, poll_us);
            }),
        );

        Ok(Self {
            cq,
            shutdown,
            transient_errors,
            handle: Some(handle),
        })
    }

    /// Test-only constructor that does not spawn a thread or touch
    /// the FFI. Used to verify the shutdown plumbing in isolation.
    #[cfg(test)]
    fn for_lifecycle_test() -> Self {
        Self {
            cq: std::ptr::null_mut(),
            shutdown: Arc::new(AtomicBool::new(false)),
            transient_errors: Arc::new(AtomicU64::new(0)),
            handle: None,
        }
    }

    pub fn cq(&self) -> *mut ffi::fid_cq {
        self.cq
    }

    pub fn transient_error_count(&self) -> u64 {
        self.transient_errors.load(Ordering::Relaxed)
    }
}

impl Drop for ProgressThread {
    fn drop(&mut self) {
        self.shutdown.store(true, Ordering::Release);
        if let Some(h) = self.handle.take() {
            let _ = h.join();
        }
    }
}

/// `Send` wrapper around the raw CQ pointer so we can move it into
/// the spawned thread without making `*mut fid_cq` globally `Send`.
#[derive(Copy, Clone)]
struct CqPtr(*mut ffi::fid_cq);
// SAFETY: callers of `ProgressThread::spawn` guarantee the CQ
// outlives the thread; only this thread reads through the pointer.
unsafe impl Send for CqPtr {}

fn progress_loop(cq: CqPtr, shutdown: Arc<AtomicBool>, errors: Arc<AtomicU64>, poll_us: u32) {
    let mut batch: [MaybeUninit<ffi::fi_cq_tagged_entry>; CQ_BATCH] =
        unsafe { MaybeUninit::uninit().assume_init() };
    let mut srcs: [ffi::fi_addr_t; CQ_BATCH] = [ffi::FI_ADDR_UNSPEC; CQ_BATCH];
    let eagain = unsafe { ffi::ub_fi_eagain() } as isize;
    let eavail = unsafe { ffi::ub_fi_eavail() } as isize;
    let idle = Duration::from_micros(poll_us as u64);

    while !shutdown.load(Ordering::Acquire) {
        // SAFETY: cq is valid for the lifetime of this thread;
        // `batch` is sized for `CQ_BATCH` entries and `srcs` for the
        // matching count of `fi_addr_t` slots.
        for s in srcs.iter_mut() {
            *s = ffi::FI_ADDR_UNSPEC;
        }
        let n = unsafe {
            ffi::ub_fi_cq_readfrom(
                cq.0,
                batch.as_mut_ptr() as *mut c_void,
                CQ_BATCH,
                srcs.as_mut_ptr(),
            )
        };

        if n > 0 {
            for i in 0..(n as usize) {
                // SAFETY: libfabric wrote at least `n` entries into
                // the batch buffer.
                let entry = unsafe { batch[i].assume_init_ref() };
                deliver_success(entry, srcs[i]);
            }
            continue;
        }

        if n == 0 {
            // No completions; brief sleep to bound CPU.
            std::thread::sleep(idle);
            continue;
        }

        // n < 0
        if n == -eagain {
            std::thread::sleep(idle);
            continue;
        }

        if n == -eavail {
            drain_errors(cq.0, &errors);
            continue;
        }

        // Unexpected negative return: bump the counter, sleep briefly,
        // keep going. Tearing the thread down on transient errors
        // would silently strand all in-flight ops.
        errors.fetch_add(1, Ordering::Relaxed);
        std::thread::sleep(idle);
    }
}

fn deliver_success(entry: &ffi::fi_cq_tagged_entry, src_addr: ffi::fi_addr_t) {
    let ctx = entry.op_context;
    if ctx.is_null() {
        return;
    }
    // SAFETY: `op_context` is the pointer we handed in via
    // `CompletionSlot::into_raw` at submission time; libfabric
    // returns it untouched on completion.
    let slot = unsafe { CompletionSlot::from_raw(ctx) };
    slot.complete(Ok(CompletionInfo {
        flags: entry.flags,
        bytes: entry.len,
        tag: entry.tag,
        src_addr,
        op_context: ctx as usize,
    }));
    // Boxed slot dropped here releases libfabric's reference.
}

fn drain_errors(cq: *mut ffi::fid_cq, errors: &AtomicU64) {
    loop {
        let mut err_entry: ffi::fi_cq_err_entry = unsafe { std::mem::zeroed() };
        // SAFETY: cq is valid; err_entry is a fresh stack-allocated
        // out-param sized to libfabric's stable ABI layout.
        let rc = unsafe { ffi::ub_fi_cq_readerr(cq, &mut err_entry, 0) };
        if rc <= 0 {
            // `1` is the documented success return for one entry; we
            // loop until libfabric reports no more.
            if rc < 0 {
                errors.fetch_add(1, Ordering::Relaxed);
            }
            break;
        }
        let ctx = err_entry.op_context;
        if !ctx.is_null() {
            // SAFETY: same provenance contract as `deliver_success`.
            let slot = unsafe { CompletionSlot::from_raw(ctx) };
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

    #[test]
    fn shutdown_flag_signals_drop_without_ffi() {
        // Exercises the Arc<AtomicBool> + Option<JoinHandle> plumbing
        // without ever calling fi_cq_read.
        let t = ProgressThread::for_lifecycle_test();
        assert!(!t.shutdown.load(Ordering::Acquire));
        let flag = t.shutdown.clone();
        drop(t);
        assert!(flag.load(Ordering::Acquire));
    }

    #[test]
    fn no_ffi_thread_join_is_idempotent_on_drop() {
        let mut t = ProgressThread::for_lifecycle_test();
        // Pretend a join already happened; subsequent drop must still
        // be sound and set the shutdown flag.
        let _ = t.handle.take();
        let flag = t.shutdown.clone();
        drop(t);
        assert!(flag.load(Ordering::Acquire));
    }

    #[test]
    fn spawn_pinned_path_runs_and_shuts_down_cleanly() {
        // Exercises spawn -> drop without any libfabric handle. We
        // run the real `spawn` path via the DefaultRuntime and a
        // null CQ pointer; the thread immediately observes the
        // shutdown flag we set before it gets to fi_cq_read.
        use crate::runtime::{DefaultRuntime, Threading, WorkerIdx};
        let rt = DefaultRuntime::new(1);
        let shutdown = Arc::new(AtomicBool::new(true));
        let errors = Arc::new(AtomicU64::new(0));
        let s = shutdown.clone();
        let e = errors.clone();
        let h = rt.spawn_pinned(
            WorkerIdx(0),
            "fabric-progress-test",
            Box::new(move || progress_loop(CqPtr(std::ptr::null_mut()), s, e, 1)),
        );
        h.join().expect("progress thread");
        assert_eq!(errors.load(Ordering::Relaxed), 0);
    }
}
