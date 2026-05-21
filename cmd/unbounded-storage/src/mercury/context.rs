// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-context Mercury state.
//!
//! A `ProgressContext` owns one `hg_context_t`, the dedicated thread that
//! drives `HG_Progress` / `HG_Trigger` against it, the per-context
//! `CompletionRegistry` for client-side back-pressured slots, and an
//! optional `ServerJobQueue` used by listening contexts to feed inbound
//! RPCs into the async server loop.
//!
//! Lifecycle is two-phase: `signal_shutdown` flips the flag the progress
//! loop polls; `join` then blocks until that thread has drained and
//! exited. `Nic` calls these in order across all of its contexts before
//! freeing peers and finalizing the class.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use super::config::NicConfig;
use super::error::{HgError, Result};
use super::ffi::{self, hg_class_t, hg_context_t};
use super::progress::{CompletionRegistry, ServerJobQueue, progress_loop};
use super::rpc::BulkGetIn;
use crate::runtime::{JoinHandle, Threading, WorkerIdx};

/// Job pushed onto a context's queue by the inbound RPC callback.
///
/// Owns the Mercury handle and the heap-allocated decoded input. The
/// async server task takes the job out of the queue, performs the bulk
/// transfer, then issues `HG_Respond` and destroys the handle.
pub struct ServerJob {
    pub handle: ffi::hg_handle_t,
    pub input: Box<BulkGetIn>,
}

// SAFETY: a `ServerJob` is produced by the RPC callback and consumed
// exactly once by the server task; nothing else aliases either field.
// Mercury handles are documented as safe to operate on from a different
// thread than the one that produced them as long as ownership is not
// shared.
unsafe impl Send for ServerJob {}

/// Per-progress-slot Mercury context plus its dedicated progress thread.
pub struct ProgressContext {
    raw: hg_context_t,
    ctx_id: u16,
    shutdown: Arc<AtomicBool>,
    join: Mutex<Option<JoinHandle>>,
    registry: Arc<CompletionRegistry>,
    server_queue: Option<Arc<ServerJobQueue<ServerJob>>>,
}

// SAFETY: `raw` is a Mercury context pointer. Mercury permits the
// dedicated progress thread to drive `HG_Progress` / `HG_Trigger` while
// other threads (the controlling thread) build/destroy and submit work.
// The interior `Mutex` on `join` plus the atomic `shutdown` flag are the
// only mutating points; everything else is read-only after construction.
unsafe impl Send for ProgressContext {}
unsafe impl Sync for ProgressContext {}

impl ProgressContext {
    /// Create a context bound to `class` and spawn its progress thread
    /// pinned to the same NUMA node as `worker`.
    ///
    /// If `cfg.listen` is true the context also owns a `ServerJobQueue`
    /// sized to `cfg.max_in_flight_per_ctx`.
    pub fn new(
        class: hg_class_t,
        ctx_id: u16,
        cfg: &NicConfig,
        threading: &dyn Threading,
        worker: WorkerIdx,
    ) -> Result<Arc<Self>> {
        if class.is_null() {
            return Err(HgError::BadConfig("ProgressContext::new: null class"));
        }

        // SAFETY: `class` is a non-null Mercury class pointer owned by
        // the caller (`Nic`); `HG_Context_create` writes through the
        // returned pointer and we wrap it before returning.
        let raw = unsafe { ffi::HG_Context_create(class) };
        if raw.is_null() {
            return Err(HgError::HgInit(0));
        }

        let registry = CompletionRegistry::new(cfg.max_in_flight_per_ctx as usize);
        let server_queue = if cfg.listen {
            Some(ServerJobQueue::<ServerJob>::new(
                cfg.max_in_flight_per_ctx as usize,
            ))
        } else {
            None
        };

        let shutdown = Arc::new(AtomicBool::new(false));
        let cfg_clone = cfg.clone();
        let raw_for_thread = ContextPtr(raw);
        let shutdown_for_thread = Arc::clone(&shutdown);
        let name = format!("hg-progress-{ctx_id}");

        let closure = move || {
            // Force whole-struct capture so `ContextPtr`'s `Send` impl
            // applies; with disjoint captures the compiler would
            // otherwise capture the inner `*mut` pointer field
            // directly, which is `!Send`.
            let owned_ptr = raw_for_thread;
            if let Err(e) = progress_loop(owned_ptr.0, &shutdown_for_thread, &cfg_clone) {
                eprintln!("hg-progress-{ctx_id}: progress_loop exited with {e}");
            }
        };
        let join = threading.spawn_aux(worker, &name, Box::new(closure));

        Ok(Arc::new(Self {
            raw,
            ctx_id,
            shutdown,
            join: Mutex::new(Some(join)),
            registry,
            server_queue,
        }))
    }

    /// Signal the progress thread to exit at its next iteration.
    /// Idempotent. Does not block.
    pub fn signal_shutdown(&self) {
        self.shutdown.store(true, Ordering::Release);
    }

    /// Block until the progress thread has exited. Idempotent: a second
    /// call after the first successful join is a no-op.
    ///
    /// Must be preceded by `signal_shutdown`; otherwise the thread will
    /// never observe the shutdown flag.
    pub fn join(&self) -> Result<()> {
        let handle = {
            let mut g = self
                .join
                .lock()
                .expect("ProgressContext join mutex poisoned");
            g.take()
        };
        if let Some(h) = handle
            && h.join().is_err()
        {
            return Err(HgError::BadConfig("progress thread panicked"));
        }
        Ok(())
    }

    pub fn raw(&self) -> hg_context_t {
        self.raw
    }

    pub fn ctx_id(&self) -> u16 {
        self.ctx_id
    }

    pub fn registry(&self) -> &Arc<CompletionRegistry> {
        &self.registry
    }

    pub fn server_queue(&self) -> Option<&Arc<ServerJobQueue<ServerJob>>> {
        self.server_queue.as_ref()
    }
}

impl Drop for ProgressContext {
    fn drop(&mut self) {
        // Best-effort safety net. `Nic::shutdown` is the documented
        // teardown path; this only fires if the controlling thread
        // dropped the `Arc<ProgressContext>` without calling it.
        self.signal_shutdown();
        if let Some(q) = self.server_queue.as_ref() {
            q.close();
        }
        let handle = {
            let mut g = match self.join.lock() {
                Ok(g) => g,
                Err(p) => p.into_inner(),
            };
            g.take()
        };
        if let Some(h) = handle {
            let _ = h.join();
        }
        if !self.raw.is_null() {
            // SAFETY: `raw` was produced by `HG_Context_create` in
            // `new` and has not been destroyed yet (we are inside Drop
            // and the progress thread has been joined above).
            unsafe {
                let _ = ffi::HG_Context_destroy(self.raw);
            }
        }
    }
}

/// `Send` shim for the raw context pointer carried into the progress
/// thread closure. The pointer itself is `*mut`, which is `!Send`; we
/// wrap it in a tuple struct and assert `Send` because the receiving
/// thread is the only one that dereferences it for the duration of the
/// `progress_loop`, after which it returns and the controlling thread
/// regains exclusive access for `HG_Context_destroy`.
struct ContextPtr(hg_context_t);
// SAFETY: see comment on `ContextPtr` above.
unsafe impl Send for ContextPtr {}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::DefaultRuntime;
    use std::ffi::CString;

    fn na_sm_class() -> Option<hg_class_t> {
        let info = CString::new("na+sm").unwrap();
        // SAFETY: `info` outlives the call; `HG_Init` reads the C string
        // and either returns a non-null class or null on failure.
        let class = unsafe { ffi::HG_Init(info.as_ptr(), ffi::HG_FALSE) };
        if class.is_null() { None } else { Some(class) }
    }

    fn finalize(class: hg_class_t) {
        // SAFETY: `class` was produced by `HG_Init` and no contexts
        // remain (callers drop them before calling `finalize`).
        unsafe {
            let _ = ffi::HG_Finalize(class);
        }
    }

    #[test]
    fn signal_shutdown_is_idempotent_and_joins_cleanly() {
        let class = match na_sm_class() {
            Some(c) => c,
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };
        let cfg = NicConfig {
            na_info_string: "na+sm".to_string(),
            ..NicConfig::default()
        };
        let rt = DefaultRuntime::new(1);
        let ctx =
            ProgressContext::new(class, 0, &cfg, &*rt, WorkerIdx(0)).expect("context construct");
        assert_eq!(ctx.ctx_id(), 0);
        assert!(ctx.server_queue().is_none(), "non-listening => no queue");
        assert!(!ctx.raw().is_null());

        ctx.signal_shutdown();
        ctx.signal_shutdown();
        ctx.join().expect("first join ok");
        ctx.join().expect("second join is a no-op");

        // Drop the context (it now owns no thread) before finalize.
        drop(ctx);
        finalize(class);
    }

    #[test]
    fn listening_context_has_server_queue() {
        let class = match na_sm_class() {
            Some(c) => c,
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };
        let cfg = NicConfig {
            na_info_string: "na+sm".to_string(),
            listen: true,
            ..NicConfig::default()
        };
        let rt = DefaultRuntime::new(1);
        let ctx =
            ProgressContext::new(class, 1, &cfg, &*rt, WorkerIdx(0)).expect("context construct");
        assert!(ctx.server_queue().is_some());
        assert_eq!(ctx.ctx_id(), 1);

        ctx.signal_shutdown();
        ctx.join().expect("join ok");

        drop(ctx);
        finalize(class);
    }
}
