// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Server-side glue for the `ub.bufferpool.bulk_get.v1` RPC.
//!
//! Sits opposite [`MercuryTransport`]: when a class is constructed
//! with `listen = true`, the RPC handler runs on the progress
//! thread, allocates a heap [`rpc::BulkGetIn`], decodes into it via
//! `HG_Get_input`, and pushes a [`ServerJob`] onto the per-class
//! [`ServerJobQueue`]. The executor-side server task polls the
//! queue, dispatches to a [`BulkSource`], pushes the resulting
//! bytes into the requester's pre-registered bulk handle, and
//! responds.
//!
//! The chained pull-through model lives on top of this: the
//! embedder supplies a `BulkSource` whose `fetch` may itself call
//! back into the local bufferpool, which (on a miss) issues another
//! [`MercuryTransport::bulk_get`] to the next hop.

use std::sync::Arc;

use serde::de::DeserializeOwned;

use crate::mercury::class::{Class, ClassInner};
use crate::mercury::error::{HgError, Result, check};
use crate::mercury::ffi;
use crate::mercury::handle::Handle;
use crate::mercury::progress::{
    Oneshot, ServerJob, ServerJobQueue, UnsafeSendPtr, complete_oneshot,
};
use crate::mercury::rpc::{BulkGetIn, BulkGetOut};

/// Application-provided source for server-side fetches. The pool
/// embedder implements this; in a chained topology it is just a
/// thin shim around the local `BufferPool::read` path.
pub trait BulkSource<R>: 'static
where
    R: DeserializeOwned,
{
    /// Provide `len` bytes for the request starting at `stripe_off`.
    /// The returned vector is consumed by the server: it is pushed
    /// into the requester's bulk handle and then dropped.
    fn fetch(
        &self,
        req: R,
        stripe_off: u64,
        len: u32,
    ) -> impl std::future::Future<Output = std::result::Result<Vec<u8>, HgError>>;
}

/// Server task driver. Hold this for as long as the class is
/// listening; dropping it stops accepting new jobs but does not
/// finalize the class.
pub struct MercuryServer<R, B>
where
    R: DeserializeOwned,
    B: BulkSource<R>,
{
    class: Arc<ClassInner>,
    queue: Arc<ServerJobQueue>,
    source: Arc<B>,
    _r: std::marker::PhantomData<fn() -> R>,
}

impl<R, B> MercuryServer<R, B>
where
    R: DeserializeOwned,
    B: BulkSource<R>,
{
    /// Wrap a listening [`Class`] with a source. Returns `None` if
    /// the class was constructed without `listen = true`.
    pub fn new(class: &Class, source: B) -> Option<Self> {
        let queue = class.server_queue()?;
        Some(Self {
            class: class.inner().clone(),
            queue,
            source: Arc::new(source),
            _r: std::marker::PhantomData,
        })
    }

    /// Drive the server loop. Pulls one job at a time and awaits
    /// `handle_job` so the pool's executor sees one in-flight RPC
    /// per server task. Returns when the queue is closed (i.e. the
    /// class is shutting down).
    pub async fn run(self) {
        loop {
            let Some(job) = self.queue.next_job().await else {
                return;
            };
            // Errors are logged via the response status; nothing to
            // raise up the stack here.
            let _ = handle_job(self.class.clone(), self.source.clone(), job).await;
        }
    }
}

/// Per-job dispatch. Decodes the request, invokes the source,
/// pushes the bytes, and responds.
async fn handle_job<R, B>(class: Arc<ClassInner>, source: Arc<B>, job: ServerJob) -> Result<()>
where
    R: DeserializeOwned,
    B: BulkSource<R>,
{
    // SAFETY: `server_rpc_cb` transferred ownership of the handle into the
    // job. We now own it and will destroy it by dropping `handle` at the
    // end of this function (via `respond`).
    let handle = unsafe { Handle::from_raw(job.handle.0 as ffi::hg_handle_t) };
    let input_ptr = job.input_struct.0 as *mut BulkGetIn;

    // Recover the decoded input. `req_bytes` is a `malloc`'d buffer
    // owned by the proc; we copy it before calling `HG_Free_input`
    // so the source's `await` lifetime is decoupled from Mercury's.
    // SAFETY: the RPC callback decoded into `*input_ptr` via
    // `HG_Get_input`; we have exclusive access until `HG_Free_input`
    // runs.
    let (stripe_off, dst_offset, len, req_bytes_vec, dst_bulk) = unsafe {
        let in_ref = &*input_ptr;
        let len = in_ref.req_bytes_len as usize;
        let req_bytes_vec = if len > 0 && !in_ref.req_bytes.is_null() {
            std::slice::from_raw_parts(in_ref.req_bytes, len).to_vec()
        } else {
            Vec::new()
        };
        (
            in_ref.stripe_off,
            in_ref.dst_offset,
            in_ref.len,
            req_bytes_vec,
            in_ref.dst_bulk,
        )
    };
    // NOTE: do NOT call `HG_Free_input` yet. The proc's `HG_FREE`
    // op frees the decoded `dst_bulk` handle as well, which we
    // still need to drive `HG_Bulk_transfer` below. We defer
    // `HG_Free_input` until after the push completes.

    let outcome = run_fetch_and_respond(
        class,
        source,
        &handle,
        stripe_off,
        dst_offset,
        len,
        req_bytes_vec,
        dst_bulk,
    )
    .await;

    // Safe to free the decoded input now; the bulk transfer is complete
    // (success or failure) and we no longer touch `dst_bulk`.
    // SAFETY: `input_ptr` came from `Box::into_raw` in `server_rpc_cb`;
    // reclaim and drop exactly once after freeing the proc-owned fields.
    handle.free_input(unsafe { &mut *input_ptr });
    unsafe {
        let _ = Box::from_raw(input_ptr);
    }

    // Respond with the final status. A failure to bincode-decode,
    // fetch, or push surfaces as a non-zero status code; the client
    // future maps it to `HgError`.
    let status: i32 = match outcome {
        Ok(()) => 0,
        Err(e) => {
            if e.code == 0 {
                -1
            } else {
                e.code
            }
        }
    };
    respond(handle, status).await
}

/// Glues fetch -> push -> respond. Splits out from `handle_job`
/// only because the failure paths above still need to respond.
async fn run_fetch_and_respond<R, B>(
    class: Arc<ClassInner>,
    source: Arc<B>,
    handle: &Handle,
    stripe_off: u64,
    dst_offset: u64,
    len: u32,
    req_bytes: Vec<u8>,
    dst_bulk: ffi::hg_bulk_t,
) -> Result<()>
where
    R: DeserializeOwned,
    B: BulkSource<R>,
{
    let req: R = bincode::deserialize(&req_bytes)
        .map_err(|_| HgError::new(0, "server bincode decode failed"))?;
    let bytes = source.fetch(req, stripe_off, len).await?;
    if bytes.len() < len as usize {
        return Err(HgError::new(0, "BulkSource returned short read"));
    }
    // The requester only owns `len` bytes at `dst_offset`; cap the
    // transfer at `len` even if the source over-produced so we never
    // push past the end of the requester's bulk handle.
    let push_len = len as usize;

    // Origin (requester) address for the push.
    // SAFETY: handle is live; shim returns a stable info pointer.
    let info = unsafe { ffi::ub_handle_info(handle.as_raw()) };
    if info.is_null() {
        return Err(HgError::new(0, "ub_handle_info returned null"));
    }
    let origin_addr = unsafe { (*info).addr };

    // Register a temporary bulk handle covering exactly `push_len`
    // bytes of the source buffer.
    // SAFETY: bytes is pinned by the surrounding await frame for
    // the duration of HG_Bulk_transfer; we drop it only after the
    // push callback fires.
    let mut bufs: [*mut std::ffi::c_void; 1] = [bytes.as_ptr() as *mut _];
    let sizes: [ffi::hg_size_t; 1] = [push_len as ffi::hg_size_t];
    let mut local_handle: ffi::hg_bulk_t = std::ptr::null_mut();
    let ret = unsafe {
        ffi::HG_Bulk_create(
            class.hg_class,
            1,
            bufs.as_mut_ptr(),
            sizes.as_ptr(),
            ffi::HG_BULK_READ_ONLY,
            &mut local_handle,
        )
    };
    check(ret, "HG_Bulk_create (server)")?;

    // Issue the push and await it via a one-shot completion slot.
    let oneshot = Oneshot::new();
    let mut op_id: ffi::hg_op_id_t = std::ptr::null_mut();
    let ret = unsafe {
        ffi::HG_Bulk_transfer(
            class.hg_context,
            Some(bulk_push_cb),
            oneshot.arg,
            ffi::HG_BULK_PUSH,
            origin_addr,
            dst_bulk,
            dst_offset,
            local_handle,
            0,
            push_len as ffi::hg_size_t,
            &mut op_id,
        )
    };
    if ret != ffi::HG_SUCCESS {
        // The callback won't run; reclaim the leaked Arc and free the
        // local bulk handle.
        // SAFETY: `oneshot.arg` was produced by `Oneshot::new` above.
        unsafe {
            Oneshot::reclaim(oneshot.arg);
            let _ = ffi::HG_Bulk_free(local_handle);
        }
        return Err(HgError::new(ret as i32, "HG_Bulk_transfer (push)"));
    }

    let push_result = oneshot.future.await;

    // SAFETY: local bulk handle is freed exactly once here, after
    // the push callback has fired.
    unsafe {
        let _ = ffi::HG_Bulk_free(local_handle);
    }
    drop(bytes);
    push_result
}

/// Issues `HG_Respond` and awaits its callback. The handle is destroyed
/// by dropping it at the end of this function regardless of success or
/// failure.
async fn respond(handle: Handle, status: i32) -> Result<()> {
    let mut out = BulkGetOut { status };
    let oneshot = Oneshot::new();
    if let Err(e) = handle.respond(respond_cb, oneshot.arg, &mut out) {
        // SAFETY: HG_Respond failed; the callback won't run. Reclaim the
        // leaked Arc. `handle` drops here -> `HG_Destroy`.
        unsafe { Oneshot::reclaim(oneshot.arg) };
        return Err(e);
    }
    // `handle` drops at the end of this scope -> `HG_Destroy`.
    oneshot.future.await
}

/// Bulk-push callback. Resolves the push completion slot with the
/// transfer's return code. Runs on the progress thread.
unsafe extern "C" fn bulk_push_cb(info: *const ffi::hg_cb_info) -> ffi::hg_return_t {
    ffi::dispatch_simple_cb(info, |ret, arg| unsafe {
        complete_oneshot(ret, arg, "bulk push failed")
    })
}

/// Respond callback. Same pattern as `bulk_push_cb`.
unsafe extern "C" fn respond_cb(info: *const ffi::hg_cb_info) -> ffi::hg_return_t {
    ffi::dispatch_simple_cb(info, |ret, arg| unsafe {
        complete_oneshot(ret, arg, "respond failed")
    })
}

/// Mercury RPC callback. Runs on the progress thread inside
/// `HG_Trigger`. Decodes the input into a fresh heap allocation and
/// queues the job for the executor-side server task.
pub(crate) unsafe extern "C" fn server_rpc_cb(handle: ffi::hg_handle_t) -> ffi::hg_return_t {
    // Wrap the raw handle immediately so every error path destroys it
    // via `Drop` instead of by hand.
    // SAFETY: Mercury passes ownership of the handle to the RPC callback.
    let handle = unsafe { Handle::from_raw(handle) };

    // SAFETY: shim returns a stable info pointer for the lifetime of the
    // handle.
    let info = unsafe { ffi::ub_handle_info(handle.as_raw()) };
    if info.is_null() {
        return -1;
    }
    let (hg_class, rpc_id) = unsafe { ((*info).hg_class, (*info).id) };
    // SAFETY: `hg_class` and `rpc_id` came from a live handle.
    let data = unsafe { ffi::HG_Registered_data(hg_class, rpc_id) };
    if data.is_null() {
        return -1;
    }
    // The Arc lives until HG_Finalize via the free callback; we need a
    // borrowed view here, not an owned clone of the slot.
    // SAFETY: `data` is the `Arc::into_raw` pointer registered at class
    // construction time.
    let queue_ref: &ServerJobQueue = unsafe { &*(data as *const ServerJobQueue) };

    // Allocate the input struct on the heap; HG_Get_input decodes into it.
    let input = Box::new(BulkGetIn::zeroed());
    let in_ptr = Box::into_raw(input);
    // SAFETY: `in_ptr` is exclusively owned by us until we hand it to the
    // queue below.
    if let Err(e) = handle.get_input(unsafe { &mut *in_ptr }) {
        // Reclaim the Box; `handle` drops -> `HG_Destroy`.
        // SAFETY: `in_ptr` came from `Box::into_raw` and has not been freed.
        unsafe {
            let _ = Box::from_raw(in_ptr);
        }
        return e.code as ffi::hg_return_t;
    }

    // Transfer handle ownership to the queue; the executor-side server
    // task will reconstruct a `Handle` in `handle_job`.
    let raw_handle = handle.into_raw();
    queue_ref.push(ServerJob {
        handle: UnsafeSendPtr(raw_handle as *mut std::ffi::c_void),
        input_struct: UnsafeSendPtr(in_ptr as *mut std::ffi::c_void),
    });
    ffi::HG_SUCCESS
}
