// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Server side of the bulk-get RPC: `BulkSource` trait and per-context
//! drainer tasks.
//!
//! `MercuryServer` is the symmetric counterpart to `MercuryTransport`. It
//! attaches one async drainer thread per listening progress context. Each
//! drainer pops `ServerJob`s from its context's `ServerJobQueue`, decodes
//! the request, calls the embedder-supplied `BulkSource::fetch` to obtain
//! the bytes, pushes them into the client's pre-registered destination via
//! `HG_Bulk_transfer(PUSH)`, and finally responds with `HG_Respond`.
//!
//! The trampoline in `nic.rs` produces the jobs by decoding the inbound
//! `BulkGetIn`, picking a context via `CtxSelector`, and pushing onto that
//! context's queue. The trampoline finds its `Nic` state through
//! `HG_Registered_data`, which `MercuryServer::run` parks at construction
//! time.

use std::future::Future;
use std::os::raw::c_void;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll, Waker};

use serde::de::DeserializeOwned;

use crate::bufferpool::Req;

use super::bulk::LocalBulk;
use super::context::{ProgressContext, ServerJob};
use super::error::{HgError, Result, check};
use super::ffi::{
    self, HG_BULK_PUSH, HG_BULK_READ_ONLY, HG_SUCCESS, hg_cb_info, hg_class_t, hg_return_t,
};
use super::handle::Handle;
use super::nic::Nic;
use super::progress::{Oneshot, from_callback_arg};
use super::rpc::{BulkGetIn, BulkGetOut};

/// Trait the application implements to serve bytes for incoming bulk-get
/// RPCs. Implementations are typically thin shims over a local
/// `BlockStore` or in-memory cache.
///
/// Returning fewer bytes than `len` is treated as a short read; the
/// server responds with a non-zero status and skips the bulk push.
pub trait BulkSource<R>: Send + Sync + 'static
where
    R: Req + DeserializeOwned + Send + Sync + 'static,
{
    fn fetch<'a>(
        &'a self,
        req: &'a R,
        offset: u64,
        len: u32,
    ) -> Pin<Box<dyn Future<Output = std::result::Result<Vec<u8>, HgError>> + Send + 'a>>;
}

/// Server bound to a `Nic` plus a `BulkSource`.
///
/// `run` consumes the server, parks an `Arc<Nic>` clone with the class
/// via `HG_Register_data` so the inbound RPC trampoline can recover it,
/// and spawns one drainer thread per listening progress context.
pub struct MercuryServer<R, B>
where
    R: Req + DeserializeOwned + Send + Sync + 'static,
    B: BulkSource<R>,
{
    nic: Arc<Nic>,
    source: Arc<B>,
    _phantom: std::marker::PhantomData<R>,
}

impl<R, B> MercuryServer<R, B>
where
    R: Req + DeserializeOwned + Send + Sync + 'static,
    B: BulkSource<R>,
{
    pub fn new(nic: Arc<Nic>, source: Arc<B>) -> Self {
        Self {
            nic,
            source,
            _phantom: std::marker::PhantomData,
        }
    }

    /// Park the per-Nic state for the trampoline and spawn one drainer
    /// thread per listening context. Returns a handle that joins the
    /// drainers on stop.
    pub fn run(self) -> ServerHandle {
        // Park the Nic Arc so the trampoline can recover it from any
        // thread by way of HG_Registered_data. Mercury invokes the
        // free callback exactly once when the class is finalized; we
        // reclaim the leaked Arc strong ref there.
        let nic_clone = Arc::clone(&self.nic);
        let parked = Arc::into_raw(nic_clone) as *mut c_void;
        // SAFETY: `nic.class()` is non-null and live; `nic.rpc_id()` was
        // returned by `HG_Register_name` against the same class.
        // `parked` is a strong `Arc<Nic>` we just leaked; the free
        // callback below reclaims it exactly once on class finalize.
        let rc = unsafe {
            ffi::HG_Register_data(
                self.nic.class(),
                self.nic.rpc_id(),
                parked,
                Some(free_nic_arc_cb),
            )
        };
        if rc != HG_SUCCESS {
            // Reclaim the leaked Arc; we never installed it.
            // SAFETY: `parked` was just produced by Arc::into_raw
            // above and has not been observed by any other code.
            unsafe {
                let _ = Arc::from_raw(parked as *const Nic);
            }
            // Best-effort: return an empty handle. Caller still gets
            // an idempotent stop().
            return ServerHandle {
                contexts: Vec::new(),
                threads: Vec::new(),
            };
        }

        let class = ClassPtr(self.nic.class());
        let mut threads = Vec::new();
        let mut contexts: Vec<Arc<ProgressContext>> = Vec::new();
        for ctx in self.nic.contexts() {
            if ctx.server_queue().is_none() {
                continue;
            }
            let ctx_clone = Arc::clone(ctx);
            let source_clone = Arc::clone(&self.source);
            let ctx_id = ctx.ctx_id();
            let name = format!("hg-server-{ctx_id}");
            let class_for_thread = class;
            let thread = std::thread::Builder::new()
                .name(name)
                .spawn(move || {
                    let owned = class_for_thread;
                    drainer::<R, B>(ctx_clone, source_clone, owned.0)
                })
                .expect("failed to spawn server drainer thread");
            threads.push(thread);
            contexts.push(Arc::clone(ctx));
        }

        ServerHandle { contexts, threads }
    }
}

/// Handle returned by `MercuryServer::run`. Drop closes all queues and
/// joins the drainer threads as a safety net; explicit `stop` is the
/// documented teardown.
pub struct ServerHandle {
    contexts: Vec<Arc<ProgressContext>>,
    threads: Vec<std::thread::JoinHandle<()>>,
}

impl ServerHandle {
    /// Idempotent: close every server queue, then join every drainer
    /// thread. Subsequent calls are no-ops because both fields are
    /// drained on first call.
    pub fn stop(mut self) {
        self.stop_in_place();
    }

    fn stop_in_place(&mut self) {
        for ctx in self.contexts.drain(..) {
            if let Some(q) = ctx.server_queue() {
                q.close();
            }
        }
        for t in self.threads.drain(..) {
            let _ = t.join();
        }
    }
}

impl Drop for ServerHandle {
    fn drop(&mut self) {
        self.stop_in_place();
    }
}

// =====================================================================
// Drainer
// =====================================================================

/// `Send` shim for the raw class pointer carried into the drainer
/// thread closure. The class outlives every drainer because `ServerHandle::stop`
/// joins the threads before the `Nic` is finalized.
#[derive(Copy, Clone)]
struct ClassPtr(hg_class_t);
// SAFETY: Mercury's class pointer is documented as safe to use from
// any thread; we never expose interior mutability of the pointer
// itself from inside the drainer.
unsafe impl Send for ClassPtr {}

fn drainer<R, B>(ctx: Arc<ProgressContext>, source: Arc<B>, class: hg_class_t)
where
    R: Req + DeserializeOwned + Send + Sync + 'static,
    B: BulkSource<R>,
{
    let queue = ctx
        .server_queue()
        .expect("listening context must have a server queue")
        .clone();
    block_on(async move {
        loop {
            let Some(job) = queue.next_job().await else {
                return;
            };
            if let Err(e) = handle_job::<R, B>(class, &source, job).await {
                eprintln!("mercury server: job failed: {e}");
            }
        }
    });
}

async fn handle_job<R, B>(class: hg_class_t, source: &Arc<B>, job: ServerJob) -> Result<()>
where
    R: Req + DeserializeOwned + Send + Sync + 'static,
    B: BulkSource<R>,
{
    // SAFETY: `job.handle` was produced by Mercury and pushed through
    // the queue exactly once. Wrapping it now transfers ownership to
    // this stack frame; on any return the `Drop` calls `HG_Destroy`.
    let handle = unsafe { Handle::from_raw(job.handle)? };
    let mut input = *job.input;

    // Decode the request bytes that Mercury copied into `input.req_bytes`
    // during the in-proc decode pass. The pointer is owned by Mercury
    // and remains valid until `HG_Free_input` is called below.
    let req: R = if input.req_bytes.is_null() || input.req_bytes_len == 0 {
        return free_input_then(&handle, &mut input, Err(HgError::Decode("empty req")));
    } else {
        // SAFETY: `input.req_bytes` and `input.req_bytes_len` were
        // populated by Mercury's decode pass over a wire buffer that
        // remains live until `HG_Free_input` returns.
        let slice =
            unsafe { std::slice::from_raw_parts(input.req_bytes, input.req_bytes_len as usize) };
        match bincode::deserialize::<R>(slice) {
            Ok(v) => v,
            Err(_) => {
                return free_input_then(&handle, &mut input, Err(HgError::Decode("bincode")));
            }
        }
    };

    // Run the application fetch first; on success we still hold
    // `input.dst_bulk` alive because Mercury keeps it valid until we
    // free the input. On error we skip the bulk push and respond with
    // a non-zero status.
    let fetch_outcome = source.fetch(&req, input.stripe_off, input.len).await;
    let bytes = match fetch_outcome {
        Ok(b) => b,
        Err(_) => {
            return respond_status(&handle, &mut input, 1);
        }
    };

    let status: i32 = if bytes.len() < input.len as usize {
        1
    } else {
        0
    };

    if status == 0 {
        // Build a temporary read-only local bulk over `bytes`. The
        // `LocalBulk` Drop frees the registration once the push
        // completes; `bytes` outlives both because we own it on this
        // stack frame.
        // SAFETY: `bytes.as_ptr()` points to `bytes.len() >= input.len`
        // valid bytes for the duration of the await below; flags are
        // a valid bitmask; `class` is the Nic's class which is alive
        // for the lifetime of the surrounding drainer.
        let local = unsafe {
            LocalBulk::over(
                class,
                bytes.as_ptr() as *mut u8,
                input.len as usize,
                HG_BULK_READ_ONLY,
            )?
        };

        let info = handle.info();
        let origin_addr = info.addr;
        let context = info.context;

        let push_oneshot = Oneshot::new();
        let arg = push_oneshot.into_callback_arg();
        let push_fut = push_oneshot.into_future();
        // SAFETY: `context`, `origin_addr`, and `input.dst_bulk` are
        // all alive for the lifetime of the inbound handle (which we
        // own); `local` is alive for the duration of the await below;
        // `arg` is a leaked strong `Arc<CompletionSlot>` reclaimed by
        // `simple_cb` exactly once.
        let rc = unsafe {
            ffi::HG_Bulk_transfer(
                context,
                Some(simple_cb),
                arg,
                HG_BULK_PUSH,
                origin_addr,
                input.dst_bulk,
                0,
                local.as_raw(),
                0,
                input.len as u64,
                std::ptr::null_mut(),
            )
        };
        if let Err(e) = check(rc, HgError::HgBulkTransfer) {
            // Reclaim the leaked slot; the callback never fired.
            // SAFETY: `arg` was just produced by `into_callback_arg`
            // and not observed by any callback because submission
            // failed synchronously.
            unsafe {
                let _ = from_callback_arg(arg);
            }
            drop(push_fut);
            return free_input_then(&handle, &mut input, Err(e));
        }
        push_fut.await?;
        // `local` drops here -> HG_Bulk_free.
    }

    respond_status(&handle, &mut input, status)
}

/// Free `input` then return `result`. Ignores the free error so callers
/// see the original failure if any.
fn free_input_then(handle: &Handle, input: &mut BulkGetIn, result: Result<()>) -> Result<()> {
    // SAFETY: `input` was populated by an earlier `HG_Get_input` on this
    // handle (via the trampoline); freeing it releases Mercury-side
    // tail allocations.
    unsafe {
        let _ = handle.free_input(input as *mut BulkGetIn as *mut c_void);
    }
    result
}

/// Free the input, send a `BulkGetOut { status }`, and await the
/// respond callback.
fn respond_status(handle: &Handle, input: &mut BulkGetIn, status: i32) -> Result<()> {
    // SAFETY: see `free_input_then`.
    unsafe {
        let _ = handle.free_input(input as *mut BulkGetIn as *mut c_void);
    }

    let mut out = BulkGetOut { status };
    let respond_oneshot = Oneshot::new();
    let arg = respond_oneshot.into_callback_arg();
    let respond_fut = respond_oneshot.into_future();
    // SAFETY: `out` lives on this stack frame for the duration of the
    // await below; `arg` is a leaked strong `Arc<CompletionSlot>` that
    // `simple_cb` reclaims exactly once.
    let submit = unsafe {
        handle.respond(
            Some(simple_cb),
            arg,
            &mut out as *mut BulkGetOut as *mut c_void,
        )
    };
    if let Err(e) = submit {
        // SAFETY: see the matching reclaim in `handle_job`.
        unsafe {
            let _ = from_callback_arg(arg);
        }
        drop(respond_fut);
        return Err(e);
    }
    block_on_future(respond_fut)
}

// =====================================================================
// FFI callbacks
// =====================================================================

/// Bulk-transfer / respond completion callback. Reclaims the
/// `Arc<CompletionSlot>` and publishes the outcome.
unsafe extern "C" fn simple_cb(info: *const hg_cb_info) -> hg_return_t {
    ffi::dispatch_simple_cb(info, |ret, arg| {
        if arg.is_null() {
            return HG_SUCCESS;
        }
        // SAFETY: `arg` is the pointer produced by
        // `Oneshot::into_callback_arg` and has not been reclaimed
        // elsewhere because the only matching reclaim site in
        // `handle_job` / `respond_status` is mutually exclusive with
        // the callback firing.
        let slot = unsafe { from_callback_arg(arg) };
        let outcome = if ret == HG_SUCCESS {
            Ok(())
        } else {
            Err(HgError::HgBulkTransfer(ret))
        };
        slot.complete(outcome);
        HG_SUCCESS
    })
}

/// Free callback handed to `HG_Register_data` so Mercury can release the
/// parked `Arc<Nic>` when the class is finalized.
pub(super) unsafe extern "C" fn free_nic_arc_cb(data: *mut c_void) {
    if data.is_null() {
        return;
    }
    // SAFETY: `data` was produced by `Arc::into_raw(Arc<Nic>)` in
    // `MercuryServer::run` and Mercury invokes this callback exactly
    // once when the class is finalized.
    unsafe {
        let _ = Arc::from_raw(data as *const Nic);
    }
}

// =====================================================================
// TinyExec: noop-waker block_on
// =====================================================================

fn block_on<F: Future>(f: F) -> F::Output {
    block_on_future(f)
}

fn block_on_future<F: Future>(mut f: F) -> F::Output {
    // SAFETY: `f` is owned by this stack frame for the duration of the
    // call and is not moved after pinning.
    let mut f = unsafe { Pin::new_unchecked(&mut f) };
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    // No spin bound: server drainers loop until their queue is closed,
    // and individual jobs await Mercury completions whose progress is
    // driven by a separate thread.
    loop {
        match f.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => std::thread::yield_now(),
        }
    }
}

fn noop_raw_waker() -> std::task::RawWaker {
    fn no_op(_: *const ()) {}
    fn clone(_: *const ()) -> std::task::RawWaker {
        noop_raw_waker()
    }
    let vt = &std::task::RawWakerVTable::new(clone, no_op, no_op, no_op);
    std::task::RawWaker::new(std::ptr::null(), vt)
}

fn noop_waker() -> Waker {
    // SAFETY: vtable is `'static`, all functions are no-ops, the data
    // pointer is never dereferenced.
    unsafe { Waker::from_raw(noop_raw_waker()) }
}

// =====================================================================
// Tests
// =====================================================================

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::StripeKey;
    use crate::mercury::config::NicConfig;
    use crate::runtime::{DefaultRuntime, WorkerIdx};
    use serde::{Deserialize, Serialize};

    #[derive(Serialize, Deserialize)]
    struct TestReq {
        key: [u8; 32],
    }

    impl Req for TestReq {
        fn key(&self) -> StripeKey {
            StripeKey(self.key)
        }
    }

    struct StubSource;
    impl BulkSource<TestReq> for StubSource {
        fn fetch<'a>(
            &'a self,
            _req: &'a TestReq,
            _offset: u64,
            len: u32,
        ) -> Pin<Box<dyn Future<Output = std::result::Result<Vec<u8>, HgError>> + Send + 'a>>
        {
            Box::pin(async move { Ok(vec![0u8; len as usize]) })
        }
    }

    fn cfg(listen: bool, contexts: u16) -> NicConfig {
        NicConfig {
            na_info_string: "na+sm".to_string(),
            listen,
            contexts_per_nic: contexts,
            ..NicConfig::default()
        }
    }

    fn build_nic(listen: bool, contexts: u16) -> Option<Arc<Nic>> {
        let rt = DefaultRuntime::new(1);
        match Nic::new(&cfg(listen, contexts), &*rt, WorkerIdx(0)) {
            Ok(n) => Some(Arc::new(n)),
            Err(HgError::HgInit(_)) => None,
            Err(e) => panic!("unexpected Nic::new error: {e:?}"),
        }
    }

    #[test]
    fn bulk_source_trait_object_compiles() {
        // Type-check that a stub source can be boxed as a trait object
        // and lives behind an `Arc`. No runtime assertions; the goal is
        // to lock down the trait shape.
        let src: Arc<dyn BulkSource<TestReq>> = Arc::new(StubSource);
        let _: Arc<dyn BulkSource<TestReq>> = Arc::clone(&src);
    }

    #[test]
    fn server_handle_stop_is_idempotent() {
        let nic = match build_nic(true, 2) {
            Some(n) => n,
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };
        let source = Arc::new(StubSource);
        let server = MercuryServer::<TestReq, _>::new(Arc::clone(&nic), source);
        let handle = server.run();
        // First stop joins the drainer threads.
        handle.stop();
        // A separately constructed handle drops without panicking; this
        // exercises the Drop-as-stop path.
        let server2 = MercuryServer::<TestReq, _>::new(Arc::clone(&nic), Arc::new(StubSource));
        let handle2 = server2.run();
        drop(handle2);
        nic.shutdown().expect("nic shutdown ok");
    }

    #[test]
    fn run_on_non_listening_nic_spawns_no_drainers() {
        let nic = match build_nic(false, 2) {
            Some(n) => n,
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };
        let server = MercuryServer::<TestReq, _>::new(Arc::clone(&nic), Arc::new(StubSource));
        let handle = server.run();
        assert!(
            handle.threads.is_empty(),
            "no listening contexts -> no threads"
        );
        handle.stop();
        nic.shutdown().expect("nic shutdown ok");
    }
}
