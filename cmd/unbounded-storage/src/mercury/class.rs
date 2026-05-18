// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Mercury class lifecycle. One `Class` per NUMA shard. It owns
//! the `hg_class_t`, the `hg_context_t`, the registered RPC ID,
//! the pinned-backing bulk handle, the peer table, the completion
//! registry that bridges the progress thread to the client-side
//! future, and the optional server-job queue that bridges the
//! progress thread to the executor-side server task.
//!
//! Construction is the only place that allocates a thread; drop
//! tears it down cleanly. Everything else is thin wrappers over the
//! FFI.

use std::ffi::CString;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use crate::bufferpool::PeerId;
use crate::mercury::bulk::LocalBulk;
use crate::mercury::config::TransportConfig;
use crate::mercury::error::{HgError, Result, check};
use crate::mercury::ffi;
use crate::mercury::peer::PeerTable;
use crate::mercury::progress::{CompletionRegistry, ServerJobQueue};
use crate::mercury::rpc;
use crate::runtime::{JoinHandle, Threading, WorkerIdx};

/// A live Mercury class plus the progress thread driving it. Held
/// behind `Arc` so transport, server, and futures can all share a
/// single instance without ownership gymnastics.
pub struct Class {
    inner: Arc<ClassInner>,
}

pub(crate) struct ClassInner {
    pub(crate) hg_class: ffi::hg_class_t,
    pub(crate) hg_context: ffi::hg_context_t,
    pub(crate) rpc_id: ffi::hg_id_t,
    /// Wrapped in `Option` purely so `Drop` can `take()` it and run
    /// `HG_Addr_free` against the still-live class before
    /// `HG_Finalize`. In every other code path this is `Some`.
    peers: Option<PeerTable>,
    pub(crate) completions: Arc<CompletionRegistry>,
    pub(crate) server: Option<Arc<ServerJobQueue>>,
    pub(crate) local_bulk: Mutex<Option<LocalBulk>>,
    shutdown: Arc<AtomicBool>,
    progress: Mutex<Option<JoinHandle>>,
    progress_poll_ms: u32,
}

// SAFETY: `hg_class_t`, `hg_context_t`, and `hg_id_t` are documented
// as thread-safe to use from any thread once initialized. The
// progress thread is the only consumer of `HG_Progress` / `HG_Trigger`,
// per Mercury's documented threading model. The completion registry
// and server queue are themselves `Send + Sync`.
unsafe impl Send for ClassInner {}
unsafe impl Sync for ClassInner {}

impl ClassInner {
    /// Register the bufferpool's pinned backing on this class.
    /// Idempotent against the same `(base, size)`; rejected if a
    /// different region was previously registered.
    pub(crate) fn register_backing(&self, base: *mut u8, size: usize) -> Result<()> {
        let mut slot = self.local_bulk.lock().expect("local_bulk mutex");
        if let Some(existing) = slot.as_ref() {
            if existing.covers(base, size) {
                return Ok(());
            }
            return Err(HgError::new(
                0,
                "class already bound to a different backing",
            ));
        }
        let bulk = LocalBulk::register(self.hg_class, base, size, ffi::HG_BULK_READWRITE)?;
        *slot = Some(bulk);
        Ok(())
    }

    /// Resolve a peer id to its `hg_addr_t`. Always succeeds while
    /// the class is alive; the `peers` field is only `None` during
    /// `Drop`.
    pub(crate) fn peer_addr(&self, peer: PeerId) -> Result<ffi::hg_addr_t> {
        self.peers
            .as_ref()
            .expect("peer table dropped")
            .lookup(peer)
    }
}

impl Drop for ClassInner {
    fn drop(&mut self) {
        // By the time the last `Arc<ClassInner>` drops, all
        // `MercuryTransport` / `MercuryServer` handles that referenced
        // this class are gone. The `Class` wrapper has already called
        // `close()` on the server queue and set `shutdown` (see
        // `Class::drop`), but we also handle the case where someone
        // built a `ClassInner` outside that wrapper.
        if let Some(q) = self.server.as_ref() {
            q.close();
        }
        self.shutdown.store(true, Ordering::Release);
        if let Some(handle) = self.progress.lock().expect("progress mutex").take() {
            let _ = handle.join();
        }
        // Drop local_bulk and the peer table before HG_Finalize so
        // `HG_Bulk_free` / `HG_Addr_free` both run against a live
        // class. Field-drop order would run them *after* this body,
        // i.e. after `HG_Finalize`, so we must do it explicitly.
        *self.local_bulk.lock().expect("local_bulk mutex") = None;
        drop(self.peers.take());
        // SAFETY: each FFI resource was allocated by Mercury and is
        // freed exactly once here.
        unsafe {
            let _ = ffi::HG_Context_destroy(self.hg_context);
            let _ = ffi::HG_Finalize(self.hg_class);
        }
    }
}

impl Class {
    /// Build a class from `cfg`. Spawns the progress thread before
    /// returning so the class is fully active on success.
    pub fn new(cfg: TransportConfig) -> Result<Self> {
        rpc::assert_layouts().map_err(|s| HgError::new(0, s))?;

        let na_info = CString::new(cfg.na_info.as_str())
            .map_err(|_| HgError::new(0, "na_info contains NUL"))?;

        // SAFETY: `na_info` is valid for the call; we check the
        // returned pointer for null below.
        let hg_class = unsafe {
            ffi::HG_Init(
                na_info.as_ptr(),
                if cfg.listen {
                    ffi::HG_TRUE
                } else {
                    ffi::HG_FALSE
                },
            )
        };
        if hg_class.is_null() {
            return Err(HgError::new(0, "HG_Init returned null"));
        }

        // From here on we must clean up `hg_class` on any error
        // path. Wrap everything below in a guard pattern.
        let result = (|| -> Result<Class> {
            // SAFETY: hg_class is non-null and valid.
            let hg_context = unsafe { ffi::HG_Context_create(hg_class) };
            if hg_context.is_null() {
                return Err(HgError::new(0, "HG_Context_create returned null"));
            }

            // SAFETY: hg_class and the two proc callbacks are live.
            // The `rpc_cb` is `Some(server_rpc_cb)` only when the
            // class is listening, so client-only classes never
            // accept incoming RPCs.
            let rpc_cb = if cfg.listen {
                Some(crate::mercury::server::server_rpc_cb as ffi::hg_rpc_cb_t)
            } else {
                None
            };
            let rpc_id = unsafe {
                ffi::HG_Register_name(
                    hg_class,
                    rpc::RPC_NAME.as_ptr() as *const _,
                    Some(rpc::in_proc()),
                    Some(rpc::out_proc()),
                    rpc_cb,
                )
            };
            if rpc_id == 0 {
                // SAFETY: hg_context is live.
                unsafe {
                    let _ = ffi::HG_Context_destroy(hg_context);
                }
                return Err(HgError::new(0, "HG_Register_name failed"));
            }

            let peers = PeerTable::new(hg_class, &cfg.peers).map_err(|e| {
                // SAFETY: cleanup; ignore errors.
                unsafe {
                    let _ = ffi::HG_Context_destroy(hg_context);
                }
                e
            })?;

            let completions = CompletionRegistry::new(cfg.max_inflight);
            let server = if cfg.listen {
                let queue = ServerJobQueue::new();
                if let Err(e) = attach_server_queue(hg_class, rpc_id, &queue) {
                    // SAFETY: cleanup; ignore errors.
                    unsafe {
                        let _ = ffi::HG_Context_destroy(hg_context);
                    }
                    return Err(e);
                }
                Some(queue)
            } else {
                None
            };

            let shutdown = Arc::new(AtomicBool::new(false));
            let inner = Arc::new(ClassInner {
                hg_class,
                hg_context,
                rpc_id,
                peers: Some(peers),
                completions,
                server,
                local_bulk: Mutex::new(None),
                shutdown: shutdown.clone(),
                progress: Mutex::new(None),
                progress_poll_ms: cfg.progress_poll_ms,
            });

            let thread = spawn_progress_thread(
                inner.clone(),
                shutdown,
                cfg.runtime.as_ref(),
                cfg.worker_idx,
            );
            *inner.progress.lock().expect("progress mutex") = Some(thread);

            Ok(Class { inner })
        })();

        match result {
            Ok(c) => Ok(c),
            Err(e) => {
                // SAFETY: hg_class was non-null; finalize once on error.
                unsafe {
                    let _ = ffi::HG_Finalize(hg_class);
                }
                Err(e)
            }
        }
    }

    /// Register the bufferpool's pinned backing. Idempotent against
    /// the same `(base, size)`; rejected if a different region was
    /// previously registered (a class is fundamentally bound to one
    /// backing for its lifetime).
    pub fn register_backing(&self, base: *mut u8, size: usize) -> Result<()> {
        self.inner.register_backing(base, size)
    }

    /// Returns the local Mercury address (formatted) so it can be
    /// shared with peers. Only meaningful when `listen` was true.
    pub fn self_address(&self) -> Result<String> {
        let mut addr: ffi::hg_addr_t = std::ptr::null_mut();
        // SAFETY: hg_class is live; addr is an out-pointer.
        let ret = unsafe { ffi::HG_Addr_self(self.inner.hg_class, &mut addr) };
        check(ret, "HG_Addr_self")?;
        if addr.is_null() {
            return Err(HgError::new(0, "HG_Addr_self returned null"));
        }
        let mut size: ffi::hg_size_t = 0;
        // SAFETY: probing for buffer size with a null buffer is the
        // documented Mercury idiom.
        let ret = unsafe {
            ffi::HG_Addr_to_string(self.inner.hg_class, std::ptr::null_mut(), &mut size, addr)
        };
        check(ret, "HG_Addr_to_string (size probe)").map_err(|e| {
            // SAFETY: free addr before returning.
            unsafe {
                let _ = ffi::HG_Addr_free(self.inner.hg_class, addr);
            }
            e
        })?;
        let mut buf = vec![0u8; size as usize];
        // SAFETY: buf has `size` bytes capacity; Mercury writes a
        // NUL-terminated C string into it.
        let ret = unsafe {
            ffi::HG_Addr_to_string(
                self.inner.hg_class,
                buf.as_mut_ptr() as *mut _,
                &mut size,
                addr,
            )
        };
        let chk = check(ret, "HG_Addr_to_string");
        // SAFETY: free addr regardless of conversion result.
        unsafe {
            let _ = ffi::HG_Addr_free(self.inner.hg_class, addr);
        }
        chk?;
        // Strip trailing NUL.
        if let Some(nul) = buf.iter().position(|&b| b == 0) {
            buf.truncate(nul);
        }
        String::from_utf8(buf).map_err(|_| HgError::new(0, "self address not valid UTF-8"))
    }

    pub(crate) fn inner(&self) -> &Arc<ClassInner> {
        &self.inner
    }

    /// Resolve a peer id to its `hg_addr_t`.
    #[allow(dead_code)]
    pub(crate) fn peer_addr(&self, peer: PeerId) -> Result<ffi::hg_addr_t> {
        self.inner.peer_addr(peer)
    }

    /// Access the server job queue for embedders that drove
    /// `listen = true`. Returns `None` for client-only classes.
    pub fn server_queue(&self) -> Option<Arc<ServerJobQueue>> {
        self.inner.server.clone()
    }
}

impl Drop for Class {
    fn drop(&mut self) {
        // Always close the server queue and flag shutdown so any
        // outstanding `MercuryTransport` / `MercuryServer` Arcs
        // observe the wind-down. The actual FFI teardown happens in
        // `ClassInner::drop` whenever the last strong reference
        // releases, which may be now or later depending on whether
        // the transport/server outlive their owning `Class`.
        if let Some(q) = self.inner.server.as_ref() {
            q.close();
        }
        self.inner.shutdown.store(true, Ordering::Release);
    }
}

fn spawn_progress_thread(
    inner: Arc<ClassInner>,
    shutdown: Arc<AtomicBool>,
    runtime: &dyn Threading,
    worker_idx: WorkerIdx,
) -> JoinHandle {
    runtime.spawn_aux(
        worker_idx,
        "mercury-progress",
        Box::new(move || progress_loop(inner, shutdown)),
    )
}

/// Body of the Mercury progress thread. Pulled out of
/// `spawn_progress_thread` so the spawn site reads as a one-liner
/// against the runtime trait.
fn progress_loop(inner: Arc<ClassInner>, shutdown: Arc<AtomicBool>) {
    let timeout = inner.progress_poll_ms;
    while !shutdown.load(Ordering::Acquire) {
        // SAFETY: hg_context lives until shutdown is set and the
        // class is dropped, which happens after the thread joins.
        unsafe {
            let _ = ffi::HG_Progress(inner.hg_context, timeout);
            loop {
                let mut actual: std::ffi::c_uint = 0;
                let ret = ffi::HG_Trigger(inner.hg_context, 0, 16, &mut actual);
                if ret != ffi::HG_SUCCESS || actual == 0 {
                    break;
                }
            }
        }
    }
}

/// Attach `queue` to `(hg_class, rpc_id)` as the registered data so
/// the C-level RPC callback can recover it via `HG_Registered_data`
/// without a global. The queue's strong-ref count is bumped here
/// and decremented by [`free_server_queue`] at `HG_Finalize` time.
fn attach_server_queue(
    hg_class: ffi::hg_class_t,
    rpc_id: ffi::hg_id_t,
    queue: &Arc<ServerJobQueue>,
) -> Result<()> {
    let raw = Arc::into_raw(queue.clone()) as *mut std::ffi::c_void;
    // SAFETY: hg_class and rpc_id are live; raw is a freshly-leaked
    // Arc that the free callback reclaims at HG_Finalize time.
    let ret = unsafe { ffi::HG_Register_data(hg_class, rpc_id, raw, Some(free_server_queue)) };
    if ret != ffi::HG_SUCCESS {
        // SAFETY: `raw` came from `Arc::into_raw` directly above.
        unsafe {
            let _ = Arc::from_raw(raw as *const ServerJobQueue);
        }
        return Err(HgError::new(ret as i32, "HG_Register_data"));
    }
    Ok(())
}

/// Frees the `ServerJobQueue` Arc handed to `HG_Register_data`.
/// Invoked exactly once by Mercury at `HG_Finalize` time.
unsafe extern "C" fn free_server_queue(data: *mut std::ffi::c_void) {
    if data.is_null() {
        return;
    }
    // SAFETY: `data` is the Arc<ServerJobQueue> raw pointer we
    // leaked at registration time; reclaim and drop exactly once.
    unsafe {
        let _ = Arc::from_raw(data as *const ServerJobQueue);
    }
}
