// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Server-side fabric RPC: receive a request, drive a `Handler`
//! stream, RMA-write pages to the client's destination MR, and
//! ack each page.
//!
//! Wire framing (mirror of `transport.rs`, see [`super::wire`]):
//!
//! * Every message carries an 8-byte [`MsgHeader`] prefix naming its
//!   [`MsgKind`] and `request_id`. There is no tagged demux under
//!   `FI_EP_MSG`; the per-connection receive pool parses the header and
//!   the [`InboundDispatch`](super::dispatch) routes it.
//! * Client -> server: a [`MsgKind::Request`] framing a bincode-encoded
//!   [`RequestHeader`] followed by the bincode-encoded `R` body.
//! * Server -> client: one [`MsgKind::PageAck`] per RMA-written page. A
//!   full success (all `dest_pages` written) sends no terminator: the
//!   client knows `dest_pages` up front and treats "all pages acked" as
//!   end-of-stream. A [`MsgKind::ResponseEnd`] (zero payload) is sent
//!   only for a short success that wrote fewer than `dest_pages` pages
//!   (including the zero-page case); an error at any point sends a
//!   [`MsgKind::ErrorAck`].
//!
//! All server replies (acks and RMA writes) go back out the same
//! connected endpoint the request arrived on, named by the
//! [`ReplyCtx`] the dispatch hands to [`RequestSink::submit`]. Because
//! the endpoint is connected, every send and write uses
//! `dest_addr = FI_ADDR_UNSPEC`.
//!
//! **Worker model**: a fixed pool of `rpc_worker_threads` long-lived
//! OS threads is spawned at `start_rpc_server`, each pinned to the
//! shard's `worker_idx`. Inbound requests are demultiplexed on the
//! progress thread by the connection's receive pool, handed to the
//! installed [`RequestSink`], decoded, and enqueued onto a bounded
//! [`JobQueue`](super::rpc_queue::JobQueue) rather than spawning a
//! thread per request. A pool worker pulls the job, drives the handler
//! stream to completion, RMA-writes pages, and blocks on each libfabric
//! completion with a *real* waker that unparks the worker; the
//! progress-thread completion path resolves the wait immediately. Queue
//! depth is capped at `max_inflight` (back-pressure); excess requests
//! get a fast "server overloaded" `ERROR_ACK`. `RpcServerHandle::drop`
//! uninstalls the request sink, sets a shutdown flag, closes the queue,
//! and joins every worker.
//!
//! **MR strategy**: ack/terminator sends use `desc = NULL` (the
//! providers' `FI_MR_LOCAL` requirement is satisfied for these small
//! framed control messages without a registered bounce buffer). The
//! `fi_write` source uses the registered local backing MR's descriptor.

use std::collections::VecDeque;
use std::marker::PhantomData;
use std::ptr;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Duration;

use serde::Serialize;
use serde::de::DeserializeOwned;

use crate::bufferpool::{BulkRef, Req, StripeKey};
use crate::runtime::{JoinHandle, park_block_on_until, thread_waker};

use super::backing::MrHandle;
use super::completion::{CompletionFuture, CompletionInfo, CompletionSlot};
use super::dispatch::{EpPtr, ReplyCtx, RequestSink};
use super::error::{FabricError, Result as FabResult};
use super::fabric::{Fabric, FabricInner};
use super::ffi;
use super::handler::{Handler, HandlerStream};
use super::rpc_queue::{Job, JobQueue};
use super::wire::{MsgHeader, MsgKind};

/// How long a worker parks in a single slice while blocked on a
/// libfabric completion before re-checking the server shutdown flag.
/// The completion path still unparks the worker immediately on success;
/// this only bounds how long a worker waiting on a completion that may
/// never arrive (peer gone mid-write) lingers after shutdown is
/// signaled, keeping `RpcServerHandle::drop` responsive.
const SHUTDOWN_POLL_SLICE: Duration = Duration::from_millis(25);

/// Loop-protection safety net for recursive Chord-finger routing.
///
/// Real termination of a recursive lookup is guaranteed by the
/// strictly-decreasing Chord ring distance at each hop; this hop
/// limit only guards against bugs or misconfiguration that could
/// otherwise let a request circulate forever. It is seeded into the
/// request header as the initial TTL and decremented at each forward.
pub const MAX_HOPS: u32 = 64;

#[doc(hidden)]
#[derive(Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub struct RequestHeader {
    pub request_id: u32,
    /// Recycled 16-bit handle identifying the client stream that should
    /// receive page-landed signals. The server echoes it in the high 16
    /// bits of each write-with-immediate (the low 16 bits carry the page
    /// ordinal), so the client can demux landed pages back to the request.
    pub page_handle: u16,
    pub dest_mr_base: u64,
    pub dest_mr_key: u64,
    pub dest_pages: u32,
    pub src_stripe: [u8; 32],
    pub src_offset: u64,
    pub src_len: u32,
    /// Remaining hop budget for recursive forwarding. Decremented at
    /// each forward. A request received with `ttl == 0` is only
    /// rejected if it would have to be forwarded again; a node that
    /// owns the stripe still serves it locally at `ttl == 0`. The
    /// hop-limit decision is made by the handler, not the RPC layer.
    pub ttl: u32,
}

impl RequestHeader {
    #[doc(hidden)]
    pub fn new(
        request_id: u32,
        page_handle: u16,
        dest_mr: &MrHandle,
        dest_pages: u32,
        src: BulkRef,
        ttl: u32,
    ) -> Self {
        Self {
            request_id,
            page_handle,
            dest_mr_base: dest_mr.remote_base,
            dest_mr_key: dest_mr.remote_key,
            dest_pages,
            src_stripe: src.stripe.0,
            src_offset: src.offset,
            src_len: src.len,
            ttl,
        }
    }

    #[doc(hidden)]
    pub fn source(&self) -> BulkRef {
        BulkRef {
            stripe: StripeKey(self.src_stripe),
            offset: self.src_offset,
            len: self.src_len,
        }
    }

    fn encoded_len() -> Option<usize> {
        bincode::serialized_size(&Self::new(
            0,
            0,
            &MrHandle {
                mr: ptr::null_mut(),
                remote_key: 0,
                base: 0,
                remote_base: 0,
                len: 0,
            },
            0,
            BulkRef {
                stripe: StripeKey([0; 32]),
                offset: 0,
                len: 0,
            },
            0,
        ))
        .map(|n| n as usize)
        .ok()
    }
}

#[derive(serde::Serialize, serde::Deserialize)]
pub(crate) struct PageAck {
    pub page_idx: u32,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
pub(crate) enum ErrorAckCode {
    Generic,
    OriginNotFound,
}

#[derive(serde::Serialize, serde::Deserialize)]
pub(crate) struct ErrorAck {
    pub code: ErrorAckCode,
    pub message: String,
}

impl ErrorAck {
    pub(crate) fn generic(message: impl Into<String>) -> Self {
        Self {
            code: ErrorAckCode::Generic,
            message: message.into(),
        }
    }

    fn for_handler_error(error: &(dyn std::error::Error + 'static)) -> Self {
        Self {
            code: classify_handler_error(error),
            message: error.to_string(),
        }
    }
}

fn classify_handler_error(error: &(dyn std::error::Error + 'static)) -> ErrorAckCode {
    let mut current = Some(error);
    while let Some(err) = current {
        if matches!(
            err.downcast_ref::<crate::bufferpool::Error>(),
            Some(crate::bufferpool::Error::OriginNotFound)
        ) {
            return ErrorAckCode::OriginNotFound;
        }
        current = err.source();
    }

    ErrorAckCode::Generic
}

/// Per-server shared state held by the responder and each worker.
struct ServerShared {
    fabric: Arc<FabricInner>,
    /// MR covering the server's local backing. Used as the source MR
    /// for `fi_write`s; if unset every request is failed with an
    /// `ERROR_ACK`.
    local_mr: Option<MrHandle>,
    /// Page size used to compute source and destination offsets for
    /// streamed page writes. Supplied by the embedder via
    /// [`Fabric::start_rpc_server`] to match the bufferpool's actual
    /// page size.
    page_size: usize,
    /// Bounded queue feeding the persistent worker pool. The request
    /// sink (progress thread) pushes decoded requests; workers pop
    /// and serve them. Closed on shutdown so workers drain and exit.
    queue: Arc<JobQueue>,
    /// Set on shutdown. Workers check it between handler-stream polls
    /// to abandon an in-flight stream with a fast `ERROR_ACK`.
    shutdown: AtomicBool,
    /// Number of requests currently queued or being served. Bounded by
    /// `max_inflight` as the server-side back-pressure limit: a request
    /// that would exceed it is rejected with a "server overloaded"
    /// `ERROR_ACK` instead of being enqueued.
    inflight: AtomicU64,
}

/// Inbound-request sink installed on the dispatch table. Decodes each
/// `REQUEST` frame, applies back-pressure, and enqueues a job onto the
/// worker pool. Erased to `Arc<dyn RequestSink>` when installed.
struct ServerSink<R, H> {
    shared: Arc<ServerShared>,
    handler: Arc<H>,
    _marker: PhantomData<fn() -> R>,
}

impl<R, H> RequestSink for ServerSink<R, H>
where
    R: Req + DeserializeOwned + Send + 'static,
    H: Handler<R> + Send + Sync + 'static,
{
    fn submit(&self, reply: ReplyCtx, request_id: u32, body: Vec<u8>) {
        // bincode encodes RequestHeader as a fixed-width prefix because
        // every field is a fixed-width integer; ask bincode for that
        // prefix length and split the framed body.
        let header_len = RequestHeader::encoded_len().unwrap_or(0);
        if body.len() < header_len {
            return;
        }
        let header: RequestHeader = match bincode::deserialize(&body[..header_len]) {
            Ok(h) => h,
            Err(_) => return,
        };
        let req: R = match bincode::deserialize(&body[header_len..]) {
            Ok(r) => r,
            Err(_) => return,
        };

        let shared = &self.shared;
        // Back-pressure: bound the number of requests queued or being
        // served to `max_inflight`. A request over the cap is shed with
        // a fast "server overloaded" ack instead of being enqueued. This
        // runs on the progress thread, so the rejection ack is
        // fire-and-forget (see `reject_overloaded`).
        let max_inflight = shared.fabric.cfg.max_inflight as u64;
        let prev = shared.inflight.fetch_add(1, Ordering::AcqRel);
        if prev >= max_inflight {
            shared.inflight.fetch_sub(1, Ordering::AcqRel);
            crate::metrics::fabric_rpc_served(crate::metrics::Outcome::Err);
            let _ = reject_overloaded(shared, reply.ep, request_id);
            return;
        }
        crate::metrics::fabric_inflight_delta(1);

        let shared_for_job = shared.clone();
        let handler_for_job = self.handler.clone();
        let job: Job = Box::new(move || {
            run_worker::<R, H>(shared_for_job.clone(), handler_for_job, header, req, reply);
            shared_for_job.inflight.fetch_sub(1, Ordering::AcqRel);
            crate::metrics::fabric_inflight_delta(-1);
        });
        if shared.queue.push(job).is_err() {
            // Queue closed (server shutting down): the job never runs, so
            // release the in-flight reservation it would have decremented.
            shared.inflight.fetch_sub(1, Ordering::AcqRel);
            crate::metrics::fabric_inflight_delta(-1);
        }
    }
}

/// Handle returned from `Fabric::start_rpc_server`. Drop uninstalls the
/// request sink, signals shutdown to the worker pool, closes the job
/// queue, and joins every worker thread before returning, so no worker
/// outlives the handle.
pub struct RpcServerHandle {
    shared: Arc<ServerShared>,
    /// Join handles for the persistent worker pool. Owned here (not in
    /// `ServerShared`) because joining consumes them.
    workers: Vec<JoinHandle>,
}

impl Drop for RpcServerHandle {
    fn drop(&mut self) {
        // Uninstall the sink first so no new request is decoded or
        // enqueued, and so the `InboundDispatch -> ServerSink ->
        // ServerShared -> FabricInner` ownership cycle is broken (the
        // fabric can never drop while the sink is installed). Then
        // signal in-flight handlers to bail out, close the queue so idle
        // workers wake and any still-queued jobs drain, unpark every
        // worker so one blocked inside a completion wait re-polls and
        // observes the shutdown flag, and finally join so every worker
        // has stopped touching the fabric before it is torn down.
        self.shared.fabric.dispatch.uninstall_server();
        self.shared.shutdown.store(true, Ordering::Release);
        self.shared.queue.close();
        for worker in &self.workers {
            worker.thread().unpark();
        }
        for worker in self.workers.drain(..) {
            let _ = worker.join();
        }
    }
}

/// Public marker used in the module re-export list. Construct one
/// indirectly via [`Fabric::start_rpc_server`].
pub struct RpcServer;

impl Fabric {
    pub fn start_rpc_server<R, H>(
        self: &Arc<Self>,
        handler: Arc<H>,
        local_mr: Option<MrHandle>,
        page_size: usize,
    ) -> FabResult<RpcServerHandle>
    where
        R: Req + Serialize + DeserializeOwned + Send + 'static,
        H: Handler<R> + Send + Sync + 'static,
    {
        let shared = Arc::new(ServerShared {
            fabric: self.inner_arc(),
            local_mr,
            page_size,
            queue: Arc::new(JobQueue::new()),
            shutdown: AtomicBool::new(false),
            inflight: AtomicU64::new(0),
        });

        // Spawn the persistent worker pool before installing the request
        // sink so a request that lands immediately has a consumer
        // waiting. Pin to the fabric unit's configured worker set so
        // handler scratch/MR access stays NUMA-local while verbs endpoints
        // can use every serving CPU assigned to the HCA.
        let runtime = shared.fabric.cfg.runtime.clone();
        let worker_indices = if shared.fabric.cfg.rpc_worker_indices.is_empty() {
            vec![shared.fabric.cfg.worker_idx]
        } else {
            shared.fabric.cfg.rpc_worker_indices.clone()
        };
        let pool_size = shared.fabric.cfg.rpc_worker_threads.max(1);
        let mut workers = Vec::with_capacity(pool_size);
        for i in 0..pool_size {
            let queue = shared.queue.clone();
            let worker_idx = worker_indices[i % worker_indices.len()];
            workers.push(runtime.spawn_pinned(
                worker_idx,
                "fabric-rpc-worker",
                Box::new(move || worker_loop(&queue)),
            ));
        }

        // Install the inbound-request sink. From here on, requests
        // demultiplexed by any connection's receive pool are decoded and
        // enqueued by `ServerSink::submit`.
        let sink: Arc<dyn RequestSink> = Arc::new(ServerSink::<R, H> {
            shared: shared.clone(),
            handler,
            _marker: PhantomData,
        });
        self.dispatch().install_server(sink);

        Ok(RpcServerHandle { shared, workers })
    }
}

/// Body of each persistent pool thread: pull jobs and run them until
/// the queue is closed and drained.
fn worker_loop(queue: &JobQueue) {
    while let Some(job) = queue.pop_blocking() {
        job();
    }
}

fn run_worker<R, H>(
    shared: Arc<ServerShared>,
    handler: Arc<H>,
    header: RequestHeader,
    req: R,
    reply: ReplyCtx,
) where
    R: Req,
    H: Handler<R>,
{
    let ep = reply.ep;
    let started = std::time::Instant::now();

    let mut log = Some({
        let mut l = crate::obs::ReqLog::new("fabric.serve");
        l.field("peer", reply.peer.0)
            .field("req_id", header.request_id)
            .hexkey("stripe", &header.src_stripe)
            .field("off", header.src_offset)
            .field("len", header.src_len)
            .field("pages", header.dest_pages)
            .field("ttl", header.ttl);
        l
    });
    // The hop budget (`ttl`) is NOT rejected here. The RPC layer is
    // generic over the handler and does not run the Chord routing, so
    // it cannot tell an owner-serve from a forward. A node that owns
    // the requested stripe must serve locally even at `ttl == 0`. The
    // decision belongs to the handler: it is handed `header.ttl` as
    // `hops_remaining` below and rejects only when it would actually
    // have to forward with no budget left (surfaced as a handler error
    // that becomes an `ERROR_ACK`).
    let local_mr = match shared.local_mr {
        Some(m) => m,
        None => {
            let _ = send_error_ack(
                &shared,
                ep,
                header.request_id,
                ErrorAck::generic("local backing not registered"),
            );

            if let Some(mut l) = log.take() {
                l.finish_err("local backing not registered");
            }

            crate::metrics::fabric_rpc_served(crate::metrics::Outcome::Err);
            crate::metrics::fabric_rpc_duration(started.elapsed().as_secs_f64());
            return;
        }
    };

    let outcome = serve_stream(&shared, &local_mr, ep, &header, &handler, &req);

    if let Some(mut l) = log.take() {
        l.field("written", outcome.written);
        match &outcome.result {
            Ok(()) => {
                l.finish_ok();
            }
            Err(msg) => {
                l.finish_err(msg.clone());
            }
        }
    }

    match outcome.result {
        Ok(()) => {
            let pages = outcome.written as u64;
            crate::metrics::fabric_written(pages, pages * shared.page_size as u64);
            crate::metrics::fabric_rpc_served(crate::metrics::Outcome::Ok);
        }
        Err(_) => {
            crate::metrics::fabric_rpc_served(crate::metrics::Outcome::Err);
        }
    }

    crate::metrics::fabric_rpc_duration(started.elapsed().as_secs_f64());
}

/// Outcome of draining one request's handler stream to the requester.
/// `written` is the number of pages successfully posted (used for the
/// success terminator decision and metrics); `result` carries the
/// already-sent error message for logging on the failure paths.
struct ServeOutcome {
    written: u32,
    result: Result<(), String>,
}

/// One page write posted on the reverse RMA path but not yet completed.
/// Carries everything needed to re-post the write if its completion
/// comes back with a transient `ENOTCONN` while the path back to the
/// requester is still being established. The source bytes live in the
/// server's long-lived registered backing (`local_mr`), so no page
/// ownership needs to be held here; correctness instead relies on
/// `serve_stream` draining every outstanding write before the handler
/// stream is dropped.
struct Inflight {
    fut: CompletionFuture,
    src_ptr: *const std::ffi::c_void,
    len: usize,
    dest_addr: u64,
    dest_key: u64,
    data: u64,
}

/// Drive one handler stream to completion, signalling each landed page
/// to the requester. Dispatches to the provider-appropriate strategy:
/// `verbs` carries the page-ack in the immediate of a single
/// `fi_writedata` (the fast path being optimized), while the native
/// `tcp` provider does not deliver write immediates to the target and so
/// falls back to an `fi_write` plus a framed `PageAck` send per page.
fn serve_stream<R, H>(
    shared: &Arc<ServerShared>,
    local_mr: &MrHandle,
    ep: EpPtr,
    header: &RequestHeader,
    handler: &Arc<H>,
    req: &R,
) -> ServeOutcome
where
    R: Req,
    H: Handler<R>,
{
    if shared.fabric.cfg.provider.supports_write_with_imm() {
        serve_stream_writedata(shared, local_mr, ep, header, handler, req)
    } else {
        serve_stream_framed(shared, local_mr, ep, header, handler, req)
    }
}

/// `verbs` fast path. Drive one handler stream to completion,
/// RMA-writing each yielded page straight into the requester's
/// destination MR and pipelining up to `write_pipeline_depth` writes
/// before parking on the oldest completion. Each page is a single
/// `fi_writedata`: the 32-bit immediate lands the page-ack on the client
/// (no separate tagged ack send), so the per-page cost on the wire is
/// one RMA write instead of a write plus a framed ack. Sends the
/// protocol terminator (none on full success, `RESPONSE_END` on a short
/// response, `ERROR_ACK` on failure) before returning.
fn serve_stream_writedata<R, H>(
    shared: &Arc<ServerShared>,
    local_mr: &MrHandle,
    ep: EpPtr,
    header: &RequestHeader,
    handler: &Arc<H>,
    req: &R,
) -> ServeOutcome
where
    R: Req,
    H: Handler<R>,
{
    let src = header.source();
    let mut stream = handler.handle(req, src, header.ttl);
    // SAFETY: stream is owned by this stack frame and pinned for the
    // duration of serve_stream; we never move it.
    let mut stream = unsafe { std::pin::Pin::new_unchecked(&mut stream) };

    // A real parking waker, created once for this request. Production
    // handler streams drive synchronously and never register it, so the
    // bounded `park_timeout` below is the safety net that re-polls and
    // re-checks the shutdown flag; if a handler ever did register and
    // wake it, the worker would unpark promptly instead.
    let waker = thread_waker();
    let mut task_cx = std::task::Context::from_waker(&waker);

    let depth = shared.fabric.cfg.write_pipeline_depth.max(1);
    // SAFETY: local_mr.mr is owned by the live fabric for the duration of
    // this request (the server handle joins workers before releasing the
    // MR), so the descriptor stays valid across every posted write.
    let desc = unsafe { ffi::ub_fi_mr_desc(local_mr.mr) };
    // Connection-establishment retry budget shared across the request:
    // the reverse-direction writes can transiently fail with -FI_EAGAIN
    // on submit or complete with prov_errno=ENOTCONN while the path back
    // to the requester is still being set up. Both clear once the
    // progress thread finishes the handshake.
    let deadline = std::time::Instant::now() + Duration::from_secs(10);

    let mut next_idx: u32 = 0;
    let mut inflight: VecDeque<Inflight> = VecDeque::new();
    let mut stream_done = false;

    loop {
        if shared.shutdown.load(Ordering::Acquire) {
            drain_inflight(shared, &mut inflight);
            let _ = send_error_ack(
                shared,
                ep,
                header.request_id,
                ErrorAck::generic("server shutting down"),
            );
            return ServeOutcome {
                written: next_idx,
                result: Err("server shutting down".to_string()),
            };
        }

        // Fill the pipeline: keep up to `depth` page writes outstanding.
        while !stream_done && inflight.len() < depth {
            match stream.as_mut().poll_next(&mut task_cx) {
                std::task::Poll::Ready(Some(Ok(page))) => {
                    match post_page(shared, local_mr, desc, ep, header, next_idx, page, deadline) {
                        Ok(inf) => {
                            inflight.push_back(inf);
                            next_idx = next_idx.saturating_add(1);
                        }
                        Err(e) => {
                            drain_inflight(shared, &mut inflight);
                            let msg = format!("write_page: {e}");
                            if !shared.shutdown.load(Ordering::Acquire) {
                                let _ = send_error_ack(
                                    shared,
                                    ep,
                                    header.request_id,
                                    ErrorAck::generic(msg.clone()),
                                );
                            }
                            return ServeOutcome {
                                written: next_idx,
                                result: Err(msg),
                            };
                        }
                    }
                }
                std::task::Poll::Ready(Some(Err(e))) => {
                    // Drain outstanding writes before surfacing the error
                    // so the NIC is no longer reading backing memory when
                    // the handler stream (and any scratch page) is dropped.
                    drain_inflight(shared, &mut inflight);
                    let msg = format!("{e}");
                    let _ = send_error_ack(
                        shared,
                        ep,
                        header.request_id,
                        ErrorAck::for_handler_error(&e),
                    );
                    return ServeOutcome {
                        written: next_idx,
                        result: Err(msg),
                    };
                }
                std::task::Poll::Ready(None) => {
                    stream_done = true;
                }
                std::task::Poll::Pending => break,
            }
        }

        if inflight.is_empty() {
            if stream_done {
                // Full success sends no terminator (the client ends on
                // "all pages landed"); only a short/zero-page response
                // needs an explicit RESPONSE_END. See module docs.
                let terminated_by_last_ack = header.dest_pages > 0 && next_idx == header.dest_pages;
                if !terminated_by_last_ack {
                    let _ = send_response_end(shared, ep, header.request_id);
                }
                return ServeOutcome {
                    written: next_idx,
                    result: Ok(()),
                };
            }
            // Handler has no page ready yet and nothing is outstanding:
            // park briefly and re-poll (the safety-net wait; production
            // handlers drive synchronously).
            std::thread::park_timeout(Duration::from_millis(5));
            continue;
        }

        // Pipeline is full or the stream is drained: wait for the oldest
        // outstanding write to complete, then loop to refill.
        if let Err(e) = await_oldest(shared, &mut inflight, desc, ep, deadline) {
            drain_inflight(shared, &mut inflight);
            let msg = format!("write_page: {e}");
            if !shared.shutdown.load(Ordering::Acquire) {
                let _ = send_error_ack(
                    shared,
                    ep,
                    header.request_id,
                    ErrorAck::generic(msg.clone()),
                );
            }
            return ServeOutcome {
                written: next_idx,
                result: Err(msg),
            };
        }
    }
}

/// Plan and submit one page write as a single `fi_writedata`. The 32-bit
/// immediate the client receives is `(page_handle << 16) | page_ordinal`:
/// the high half echoes the request's recycled stream handle so the
/// client can demux, the low half is the destination page ordinal.
#[allow(clippy::too_many_arguments)]
fn post_page(
    shared: &Arc<ServerShared>,
    local_mr: &MrHandle,
    desc: *mut std::ffi::c_void,
    ep: EpPtr,
    header: &RequestHeader,
    dest_idx: u32,
    page: crate::bufferpool::PageRef,
    deadline: std::time::Instant,
) -> FabResult<Inflight> {
    let plan = plan_page_write(
        local_mr,
        &RequestPlan::from(header),
        dest_idx,
        page,
        shared.page_size,
    )?;
    if plan.ack_page_idx > u16::MAX as u32 {
        return Err(FabricError::BadConfig(
            "page ordinal exceeds 16-bit immediate",
        ));
    }
    let src_ptr = plan.src_addr as *const std::ffi::c_void;
    let data = ((header.page_handle as u64) << 16) | (plan.ack_page_idx as u64);
    let fut = post_writedata(
        shared,
        desc,
        ep,
        src_ptr,
        plan.len,
        data,
        plan.dest_addr,
        header.dest_mr_key,
        deadline,
    )?;
    Ok(Inflight {
        fut,
        src_ptr,
        len: plan.len,
        dest_addr: plan.dest_addr,
        dest_key: header.dest_mr_key,
        data,
    })
}

/// Submit a single `fi_writedata`, retrying `-FI_EAGAIN` submit failures
/// (transmit queue momentarily full, or the reverse connection still
/// establishing) with a short backoff until `deadline`. Returns the
/// completion future without awaiting it so the caller can pipeline.
#[allow(clippy::too_many_arguments)]
fn post_writedata(
    shared: &Arc<ServerShared>,
    desc: *mut std::ffi::c_void,
    ep: EpPtr,
    src_ptr: *const std::ffi::c_void,
    len: usize,
    data: u64,
    dest_addr: u64,
    dest_key: u64,
    deadline: std::time::Instant,
) -> FabResult<CompletionFuture> {
    loop {
        let (slot, fut) = shared.fabric.completions.allocate()?;
        let ctx = slot.into_raw();
        // SAFETY: ep and desc are owned by the live fabric; ctx is a
        // freshly boxed slot handed to libfabric, reclaimed by the
        // progress thread on completion or by us on a synchronous failure.
        let rc = unsafe {
            ffi::ub_fi_writedata(
                ep.0,
                src_ptr,
                len,
                desc,
                data,
                ffi::FI_ADDR_UNSPEC,
                dest_addr,
                dest_key,
                ctx,
            )
        };
        if rc < 0 {
            // SAFETY: just produced by into_raw and not yet completed.
            let _ = unsafe { CompletionSlot::from_raw(ctx) };
            if rc as i32 == -ffi::FI_EAGAIN && std::time::Instant::now() < deadline {
                std::thread::sleep(Duration::from_millis(1));
                continue;
            }
            return Err(FabricError::Pkg("fi_writedata", rc as i32));
        }
        return Ok(fut);
    }
}

/// Wait for the oldest outstanding page write to complete. A completion
/// carrying `prov_errno=ENOTCONN` means the connection back to the
/// requester is still being established; re-post the same write and keep
/// it at the head until it lands or the request `deadline` passes.
fn await_oldest(
    shared: &Arc<ServerShared>,
    inflight: &mut VecDeque<Inflight>,
    desc: *mut std::ffi::c_void,
    ep: EpPtr,
    deadline: std::time::Instant,
) -> FabResult<()> {
    let inf = match inflight.pop_front() {
        Some(i) => i,
        None => return Ok(()),
    };
    match block_on(inf.fut, Duration::from_secs(10), &shared.shutdown) {
        Ok(_) => Ok(()),
        Err(FabricError::Cq { prov_errno, err })
            if prov_errno == ffi::ENOTCONN && std::time::Instant::now() < deadline =>
        {
            let _ = err;
            std::thread::sleep(Duration::from_millis(1));
            let fut = post_writedata(
                shared,
                desc,
                ep,
                inf.src_ptr,
                inf.len,
                inf.data,
                inf.dest_addr,
                inf.dest_key,
                deadline,
            )?;
            inflight.push_front(Inflight { fut, ..inf });
            Ok(())
        }
        Err(e) => Err(e),
    }
}

/// Wait for every still-outstanding page write to finish (success or
/// error) so the NIC is no longer reading the server's backing memory
/// before the handler stream - and with it any scratch page - is
/// dropped. Errors are swallowed: this only runs on teardown paths where
/// the request outcome has already been decided. Bounded by the server
/// shutdown flag and per-op timeout inside `block_on`.
fn drain_inflight(shared: &Arc<ServerShared>, inflight: &mut VecDeque<Inflight>) {
    while let Some(inf) = inflight.pop_front() {
        let _ = block_on(inf.fut, Duration::from_secs(10), &shared.shutdown);
    }
}

/// `tcp` fallback path. The native tcp provider performs an
/// `fi_writedata` RMA write but never delivers the immediate to the
/// target, so the client would never see a page land. Here each page is
/// instead an `fi_write` (plain RMA) followed by a framed `MsgKind::PageAck`
/// send the client's receive pool routes back to the waiting stream.
/// Runs at queue depth 1 (write, await completion, send the ack) since
/// this path is only used by the loopback/smoke tests and is not the
/// throughput target. Terminator semantics match the writedata path.
fn serve_stream_framed<R, H>(
    shared: &Arc<ServerShared>,
    local_mr: &MrHandle,
    ep: EpPtr,
    header: &RequestHeader,
    handler: &Arc<H>,
    req: &R,
) -> ServeOutcome
where
    R: Req,
    H: Handler<R>,
{
    let src = header.source();
    let mut stream = handler.handle(req, src, header.ttl);
    // SAFETY: stream is owned by this stack frame and pinned for the
    // duration of serve_stream_framed; we never move it.
    let mut stream = unsafe { std::pin::Pin::new_unchecked(&mut stream) };

    let waker = thread_waker();
    let mut task_cx = std::task::Context::from_waker(&waker);

    let mut next_idx: u32 = 0;

    loop {
        if shared.shutdown.load(Ordering::Acquire) {
            let _ = send_error_ack(
                shared,
                ep,
                header.request_id,
                ErrorAck::generic("server shutting down"),
            );
            return ServeOutcome {
                written: next_idx,
                result: Err("server shutting down".to_string()),
            };
        }

        match stream.as_mut().poll_next(&mut task_cx) {
            std::task::Poll::Ready(Some(Ok(page))) => {
                if let Err(e) = write_page(shared, local_mr, ep, header, next_idx, page) {
                    let msg = format!("write_page: {e}");
                    if !shared.shutdown.load(Ordering::Acquire) {
                        let _ = send_error_ack(
                            shared,
                            ep,
                            header.request_id,
                            ErrorAck::generic(msg.clone()),
                        );
                    }
                    return ServeOutcome {
                        written: next_idx,
                        result: Err(msg),
                    };
                }
                next_idx = next_idx.saturating_add(1);
            }
            std::task::Poll::Ready(Some(Err(e))) => {
                let msg = format!("{e}");
                let _ = send_error_ack(
                    shared,
                    ep,
                    header.request_id,
                    ErrorAck::for_handler_error(&e),
                );
                return ServeOutcome {
                    written: next_idx,
                    result: Err(msg),
                };
            }
            std::task::Poll::Ready(None) => {
                let terminated_by_last_ack = header.dest_pages > 0 && next_idx == header.dest_pages;
                if !terminated_by_last_ack {
                    let _ = send_response_end(shared, ep, header.request_id);
                }
                return ServeOutcome {
                    written: next_idx,
                    result: Ok(()),
                };
            }
            std::task::Poll::Pending => {
                std::thread::park_timeout(Duration::from_millis(5));
            }
        }
    }
}

/// Write one page with a plain `fi_write`, block on its completion, then
/// send a framed `PageAck`. Retries `-FI_EAGAIN` submit failures and
/// transient `ENOTCONN` completions (reverse connection still
/// establishing) with a short backoff until a 10s deadline.
fn write_page(
    shared: &Arc<ServerShared>,
    local_mr: &MrHandle,
    ep: EpPtr,
    header: &RequestHeader,
    dest_idx: u32,
    page: crate::bufferpool::PageRef,
) -> FabResult<()> {
    let plan = plan_page_write(
        local_mr,
        &RequestPlan::from(header),
        dest_idx,
        page,
        shared.page_size,
    )?;
    let src_ptr = plan.src_addr as *const std::ffi::c_void;
    // SAFETY: local_mr.mr is owned by the live fabric for the duration of
    // this request, so the descriptor stays valid across the write.
    let desc = unsafe { ffi::ub_fi_mr_desc(local_mr.mr) };
    let deadline = std::time::Instant::now() + Duration::from_secs(10);

    loop {
        let (slot, fut) = shared.fabric.completions.allocate()?;
        let ctx = slot.into_raw();
        // SAFETY: ep and desc are owned by the live fabric; ctx is a
        // freshly boxed slot handed to libfabric, reclaimed by the
        // progress thread on completion or by us on a synchronous failure.
        let rc = unsafe {
            ffi::ub_fi_write(
                ep.0,
                src_ptr,
                plan.len,
                desc,
                ffi::FI_ADDR_UNSPEC,
                plan.dest_addr,
                header.dest_mr_key,
                ctx,
            )
        };
        if rc < 0 {
            // SAFETY: just produced by into_raw and not yet completed.
            let _ = unsafe { CompletionSlot::from_raw(ctx) };
            if rc as i32 == -ffi::FI_EAGAIN && std::time::Instant::now() < deadline {
                std::thread::sleep(Duration::from_millis(1));
                continue;
            }
            return Err(FabricError::Pkg("fi_write", rc as i32));
        }
        match block_on(fut, Duration::from_secs(10), &shared.shutdown) {
            Ok(_) => break,
            Err(FabricError::Cq { prov_errno, err })
                if prov_errno == ffi::ENOTCONN && std::time::Instant::now() < deadline =>
            {
                let _ = err;
                std::thread::sleep(Duration::from_millis(1));
                continue;
            }
            Err(e) => return Err(e),
        }
    }

    send_page_ack(shared, ep, header.request_id, plan.ack_page_idx)
}

/// Frame and send a `MsgKind::PageAck` naming the landed destination
/// page ordinal, awaiting the send completion.
fn send_page_ack(
    shared: &Arc<ServerShared>,
    ep: EpPtr,
    request_id: u32,
    page_idx: u32,
) -> FabResult<()> {
    let payload = bincode::serialize(&PageAck { page_idx })
        .map_err(|_| FabricError::Pkg("bincode(PageAck)", 0))?;
    submit_small_send(shared, ep, MsgKind::PageAck, request_id, &payload)
}

/// Deterministic request destination metadata used by page planning.
#[doc(hidden)]
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct RequestPlan {
    pub dest_mr_base: u64,
    pub dest_pages: u32,
}

/// Deterministic server-side write layout for one streamed page.
#[doc(hidden)]
#[derive(Debug, PartialEq, Eq)]
pub struct PageWritePlan {
    pub src_addr: usize,
    pub dest_addr: u64,
    pub len: usize,
    pub ack_page_idx: u32,
}

/// Plan the local and remote address range used for one page write.
#[doc(hidden)]
pub fn plan_page_write(
    local_mr: &MrHandle,
    request: &RequestPlan,
    dest_idx: u32,
    page: crate::bufferpool::PageRef,
    page_size: usize,
) -> FabResult<PageWritePlan> {
    if dest_idx >= request.dest_pages {
        return Err(FabricError::BadConfig(
            "destination page index out of range",
        ));
    }
    let page_offset = page.offset as usize;
    let len = page.len as usize;
    page_offset
        .checked_add(len)
        .filter(|&end| end <= page_size)
        .ok_or(FabricError::BadConfig("page range out of bounds"))?;

    let src_page_offset = (page.page_idx as usize)
        .checked_mul(page_size)
        .ok_or(FabricError::BadConfig("source page offset overflow"))?;
    let src_offset = src_page_offset
        .checked_add(page_offset)
        .ok_or(FabricError::BadConfig("source page offset overflow"))?;
    src_offset
        .checked_add(len)
        .filter(|&end| end <= local_mr.len)
        .ok_or(FabricError::BadConfig("page range out of range (local)"))?;
    let src_addr = local_mr
        .base
        .checked_add(src_offset)
        .ok_or(FabricError::BadConfig("source address overflow"))?;

    let dest_pages_len = (request.dest_pages as usize)
        .checked_mul(page_size)
        .ok_or(FabricError::BadConfig("destination length overflow"))?;
    let dest_page_offset = (dest_idx as usize)
        .checked_mul(page_size)
        .ok_or(FabricError::BadConfig("destination page offset overflow"))?;
    let dest_offset = dest_page_offset
        .checked_add(page_offset)
        .ok_or(FabricError::BadConfig("destination page offset overflow"))?;
    dest_offset
        .checked_add(len)
        .filter(|&end| end <= dest_pages_len)
        .ok_or(FabricError::BadConfig(
            "page range out of range (destination)",
        ))?;
    let dest_addr = request
        .dest_mr_base
        .checked_add(dest_offset as u64)
        .ok_or(FabricError::BadConfig("destination address overflow"))?;

    Ok(PageWritePlan {
        src_addr,
        dest_addr,
        len,
        // The client treats PageAck.page_idx as an ordinal into its
        // dsts slice, not as the backing PageRef.page_idx.
        ack_page_idx: dest_idx,
    })
}

impl From<&RequestHeader> for RequestPlan {
    fn from(header: &RequestHeader) -> Self {
        Self {
            dest_mr_base: header.dest_mr_base,
            dest_pages: header.dest_pages,
        }
    }
}

fn send_response_end(shared: &Arc<ServerShared>, ep: EpPtr, request_id: u32) -> FabResult<()> {
    submit_small_send(shared, ep, MsgKind::ResponseEnd, request_id, &[])
}

fn send_error_ack(
    shared: &Arc<ServerShared>,
    ep: EpPtr,
    request_id: u32,
    ack: ErrorAck,
) -> FabResult<()> {
    let payload = bincode::serialize(&ack).map_err(|_| FabricError::Pkg("bincode(ErrorAck)", 0))?;
    submit_small_send(shared, ep, MsgKind::ErrorAck, request_id, &payload)
}

fn submit_small_send(
    shared: &Arc<ServerShared>,
    ep: EpPtr,
    kind: MsgKind,
    request_id: u32,
    payload: &[u8],
) -> FabResult<()> {
    let fut = enqueue_small_send(
        shared,
        ep,
        kind,
        request_id,
        payload,
        Duration::from_secs(10),
    )?;
    wait_for_small_send(fut, Duration::from_secs(10), &shared.shutdown)
}

/// Frame and submit a small control message and return its completion
/// future without waiting. The framed send buffer is freed by the slot
/// handler when the send completes, so dropping the returned future is
/// safe: the completion still fires on the progress thread and reclaims
/// the buffer.
///
/// `submit_backoff` bounds the `-FI_EAGAIN` submit retry. Worker
/// threads pass a generous budget so a send survives the connection
/// still establishing; the progress thread (overload rejection) passes
/// `Duration::ZERO` for a single non-blocking attempt so it never
/// stalls completion progress.
fn enqueue_small_send(
    shared: &Arc<ServerShared>,
    ep: EpPtr,
    kind: MsgKind,
    request_id: u32,
    payload: &[u8],
    submit_backoff: Duration,
) -> FabResult<CompletionFuture> {
    let inner = &shared.fabric;
    let framed = MsgHeader::frame(kind, request_id, payload);
    inner
        .send_pool()
        .send_framed(ep.0, framed, "fi_send (small)", submit_backoff)
}

/// Shed a request the server is too busy to admit. Runs on the progress
/// thread (in the request sink), so it must not block: the ErrorAck is
/// submitted with a single non-blocking attempt and its completion is
/// not awaited. Dropping the future is safe (the slot handler frees the
/// send buffer when the completion is reaped).
fn reject_overloaded(shared: &Arc<ServerShared>, ep: EpPtr, request_id: u32) -> FabResult<()> {
    let payload = bincode::serialize(&ErrorAck::generic("server overloaded"))
        .map_err(|_| FabricError::Pkg("bincode(ErrorAck)", 0))?;
    let _fut = enqueue_small_send(
        shared,
        ep,
        MsgKind::ErrorAck,
        request_id,
        &payload,
        Duration::ZERO,
    )?;
    Ok(())
}

fn wait_for_small_send(
    fut: CompletionFuture,
    timeout: Duration,
    shutdown: &AtomicBool,
) -> FabResult<()> {
    block_on(fut, timeout, shutdown).map(|_| ())
}

/// Block the calling worker thread on a libfabric completion, parking
/// (not spinning) between polls. The progress-thread completion path
/// (`CompletionSlot::complete` -> `AtomicWaker::wake`) unparks us, so
/// the wait resolves as soon as the CQE is reaped. Returns
/// `FabricError::Timeout` if `timeout` elapses first, or as soon as
/// `shutdown` is set so a draining server does not block joining a
/// worker stuck on a completion a dead peer will never deliver.
fn block_on(
    fut: CompletionFuture,
    timeout: Duration,
    shutdown: &AtomicBool,
) -> FabResult<CompletionInfo> {
    match park_block_on_until(fut, timeout, SHUTDOWN_POLL_SLICE, || {
        shutdown.load(Ordering::Acquire)
    }) {
        Some(result) => result,
        None => Err(FabricError::Timeout),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::noop_waker;

    fn mr(base: usize, len: usize) -> MrHandle {
        MrHandle {
            mr: ptr::null_mut(),
            remote_key: 0xAABB_CCDD,
            base,
            remote_base: base as u64,
            len,
        }
    }

    fn header(dest_base: u64, dest_pages: u32) -> RequestHeader {
        RequestHeader::new(
            7,
            0,
            &mr(
                dest_base as usize,
                dest_pages as usize * crate::memory::HUGEPAGE_2MB,
            ),
            dest_pages,
            BulkRef {
                stripe: StripeKey([0; 32]),
                offset: 0,
                len: 0,
            },
            MAX_HOPS,
        )
    }

    fn request_plan(header: &RequestHeader) -> RequestPlan {
        RequestPlan::from(header)
    }

    fn completion_future(result: FabResult<CompletionInfo>) -> CompletionFuture {
        let reg = super::super::completion::CompletionRegistry::new(1);
        let (slot, fut) = reg.allocate().unwrap();
        let raw = slot.into_raw();
        // SAFETY: raw was just produced by `into_raw`.
        let reclaimed = unsafe { CompletionSlot::from_raw(raw) };
        reclaimed.complete(result);
        fut
    }

    fn completion_info() -> CompletionInfo {
        CompletionInfo {
            flags: 0,
            bytes: 0,
            tag: 0,
            src_addr: 0,
            op_context: 0,
            data: 0,
        }
    }

    #[test]
    fn wait_for_small_send_accepts_completion_success() {
        let fut = completion_future(Ok(completion_info()));

        assert!(wait_for_small_send(fut, Duration::from_secs(1), &AtomicBool::new(false)).is_ok());
    }

    #[test]
    fn wait_for_small_send_propagates_completion_error() {
        let fut = completion_future(Err(FabricError::Cq {
            prov_errno: -3,
            err: -5,
        }));

        let err =
            wait_for_small_send(fut, Duration::from_secs(1), &AtomicBool::new(false)).unwrap_err();

        match err {
            FabricError::Cq { prov_errno, err } => {
                assert_eq!(prov_errno, -3);
                assert_eq!(err, -5);
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[test]
    fn wait_for_small_send_propagates_timeout() {
        let reg = super::super::completion::CompletionRegistry::new(1);
        let (_slot, fut) = reg.allocate().unwrap();

        let err = wait_for_small_send(fut, Duration::ZERO, &AtomicBool::new(false)).unwrap_err();

        assert!(matches!(err, FabricError::Timeout));
    }

    #[test]
    fn wait_for_small_send_abandons_wait_when_shutdown_is_set() {
        // A never-completing send (slot never reaped) with a long
        // timeout must still return promptly once shutdown is set, so a
        // draining server is not blocked joining the worker.
        let reg = super::super::completion::CompletionRegistry::new(1);
        let (_slot, fut) = reg.allocate().unwrap();
        let shutdown = AtomicBool::new(true);

        let err = wait_for_small_send(fut, Duration::from_secs(3600), &shutdown).unwrap_err();

        assert!(matches!(err, FabricError::Timeout));
    }

    #[test]
    fn request_header_preserves_source_range() {
        let dest_mr = mr(0x1000, 4096);
        let src = BulkRef {
            stripe: StripeKey([0x5A; 32]),
            offset: 0x1122_3344_5566_7788,
            len: 0x99AA_BBCC,
        };

        let header = RequestHeader::new(7, 5, &dest_mr, 3, src, 13);

        assert_eq!(header.request_id, 7);
        assert_eq!(header.page_handle, 5);
        assert_eq!(header.dest_mr_base, 0x1000);
        assert_eq!(header.dest_mr_key, 0xAABB_CCDD);
        assert_eq!(header.dest_pages, 3);
        assert_eq!(header.source(), src);
    }

    #[test]
    fn request_header_round_trips_ttl_and_encoded_len_matches_prefix() {
        let dest_mr = mr(0x2000, 8192);
        let src = BulkRef {
            stripe: StripeKey([0x11; 32]),
            offset: 0xDEAD_BEEF,
            len: 0x0BAD_F00D,
        };

        let header = RequestHeader::new(9, 0, &dest_mr, 5, src, 42);
        assert_eq!(header.ttl, 42);

        let bytes = bincode::serialize(&header).expect("serialize header");
        let expected_len = RequestHeader::encoded_len().expect("encoded len");
        assert_eq!(bytes.len(), expected_len);

        let decoded: RequestHeader =
            bincode::deserialize(&bytes[..expected_len]).expect("deserialize header");
        assert_eq!(decoded, header);
        assert_eq!(decoded.ttl, 42);
    }

    #[test]
    fn page_write_plan_uses_handler_page_range_and_ack_ordinal() {
        let page_size = crate::memory::HUGEPAGE_2MB;
        let local_mr = mr(0x1000_0000, 4 * page_size);
        let header = header(0x2000_0000, 3);
        let page = crate::bufferpool::PageRef {
            page_idx: 2,
            offset: 128,
            len: 4096,
        };

        let plan = plan_page_write(&local_mr, &request_plan(&header), 1, page, page_size)
            .expect("plan page write");

        assert_eq!(plan.src_addr, local_mr.base + 2 * page_size + 128);
        assert_eq!(plan.dest_addr, header.dest_mr_base + page_size as u64 + 128);
        assert_eq!(plan.len, 4096);
        assert_eq!(plan.ack_page_idx, 1);
    }

    #[test]
    fn page_write_plan_accepts_non_sequential_source_page() {
        let page_size = crate::memory::HUGEPAGE_2MB;
        let local_mr = mr(0x3000_0000, 4 * page_size);
        let header = header(0x4000_0000, 2);
        let page = crate::bufferpool::PageRef {
            page_idx: 3,
            offset: 0,
            len: page_size as u32,
        };

        let plan = plan_page_write(&local_mr, &request_plan(&header), 0, page, page_size)
            .expect("plan page write");

        assert_eq!(plan.src_addr, local_mr.base + 3 * page_size);
        assert_eq!(plan.dest_addr, header.dest_mr_base);
        assert_eq!(plan.ack_page_idx, 0);
    }

    #[test]
    fn page_write_plan_rejects_local_range_out_of_bounds() {
        let page_size = crate::memory::HUGEPAGE_2MB;
        let local_mr = mr(0x5000_0000, page_size);
        let header = header(0x6000_0000, 1);
        let page = crate::bufferpool::PageRef {
            page_idx: 1,
            offset: 0,
            len: 1,
        };

        assert!(plan_page_write(&local_mr, &request_plan(&header), 0, page, page_size).is_err());
    }

    #[test]
    fn page_write_plan_rejects_destination_range_out_of_bounds() {
        let page_size = crate::memory::HUGEPAGE_2MB;
        let local_mr = mr(0x7000_0000, page_size);
        let header = header(0x8000_0000, 1);
        let page = crate::bufferpool::PageRef {
            page_idx: 0,
            offset: page_size as u32 - 7,
            len: 8,
        };

        assert!(plan_page_write(&local_mr, &request_plan(&header), 0, page, page_size).is_err());
        assert!(
            plan_page_write(
                &local_mr,
                &request_plan(&header),
                1,
                crate::bufferpool::PageRef {
                    page_idx: 0,
                    offset: 0,
                    len: 1,
                },
                page_size,
            )
            .is_err()
        );
    }

    // --- hop-budget dispatch (ttl == 0 must serve an owner) ---
    //
    // `run_worker` is generic over the handler and hands it
    // `header.ttl` as `hops_remaining`. These tests pin down the
    // contract `run_worker` relies on: the *handler* decides the hop
    // limit. An owner-serve must yield a page even at `ttl == 0`; a
    // forward with no budget must surface an error (which `run_worker`
    // turns into an `ERROR_ACK`).

    /// What the first item of a handler's stream tells `run_worker` to
    /// do: serve a page, reject with an error ack, or end the response.
    #[derive(Debug)]
    enum FirstOutcome {
        Served(crate::bufferpool::PageRef),
        Rejected(String),
        Ended,
    }

    /// Drive a handler stream to its first ready item exactly the way
    /// `run_worker`'s poll loop does (noop waker, blocking poll). This
    /// is the dispatch decision the `ttl == 0` fix turns on: a page
    /// means "served", an error means "rejected with an ERROR_ACK".
    fn first_outcome<R, H>(handler: &H, req: &R, ttl: u32) -> FirstOutcome
    where
        R: Req,
        H: Handler<R>,
    {
        let src = BulkRef {
            stripe: StripeKey([0; 32]),
            offset: 0,
            len: 0,
        };
        let mut stream = handler.handle(req, src, ttl);
        // SAFETY: stream is owned by this frame and never moved.
        let mut stream = unsafe { std::pin::Pin::new_unchecked(&mut stream) };
        let waker = noop_waker();
        let mut cx = std::task::Context::from_waker(&waker);
        loop {
            match stream.as_mut().poll_next(&mut cx) {
                std::task::Poll::Ready(Some(Ok(page))) => return FirstOutcome::Served(page),
                std::task::Poll::Ready(Some(Err(e))) => {
                    return FirstOutcome::Rejected(format!("{e}"));
                }
                std::task::Poll::Ready(None) => return FirstOutcome::Ended,
                // The mock handlers below resolve synchronously.
                std::task::Poll::Pending => std::thread::yield_now(),
            }
        }
    }

    struct RoutingReq(StripeKey);

    impl Req for RoutingReq {
        fn key(&self) -> StripeKey {
            self.0
        }
    }

    /// Handler error standing in for
    /// `RecursiveHandlerError::HopLimitExceeded`.
    #[derive(Debug)]
    struct HopLimit;

    impl std::fmt::Display for HopLimit {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "hop limit exceeded")
        }
    }

    impl std::error::Error for HopLimit {}

    /// Mock handler mirroring `RecursiveHandler`'s `classify`: it owns
    /// the stripe (serves regardless of budget) or it forwards
    /// (rejecting only when `hops_remaining == 0`).
    struct RoutingHandler {
        owns: bool,
    }

    struct RoutingStream {
        outcome: Option<Result<crate::bufferpool::PageRef, HopLimit>>,
    }

    impl HandlerStream for RoutingStream {
        type Error = HopLimit;
        fn poll_next(
            mut self: std::pin::Pin<&mut Self>,
            _cx: &mut std::task::Context<'_>,
        ) -> std::task::Poll<Option<Result<crate::bufferpool::PageRef, HopLimit>>> {
            std::task::Poll::Ready(self.outcome.take())
        }
    }

    impl<R: Req> Handler<R> for RoutingHandler {
        type Error = HopLimit;
        type Stream<'a>
            = RoutingStream
        where
            Self: 'a,
            R: 'a;
        fn handle<'a>(
            &'a self,
            _req: &'a R,
            _src: BulkRef,
            hops_remaining: u32,
        ) -> Self::Stream<'a> {
            let page = crate::bufferpool::PageRef {
                page_idx: 0,
                offset: 0,
                len: 4096,
            };
            let outcome = if self.owns {
                // Owner: serve from the local store regardless of the
                // remaining hop budget.
                Some(Ok(page))
            } else if hops_remaining == 0 {
                // Forward with no budget: the genuine hop-limit case.
                Some(Err(HopLimit))
            } else {
                // Forward with budget: relays a downstream page.
                Some(Ok(page))
            };
            RoutingStream { outcome }
        }
    }

    #[test]
    fn owner_serves_even_with_exhausted_ttl() {
        let handler = RoutingHandler { owns: true };
        let req = RoutingReq(StripeKey([0; 32]));

        match first_outcome(&handler, &req, 0) {
            FirstOutcome::Served(_) => {}
            other => panic!("owner must serve at ttl == 0, got {other:?}"),
        }
    }

    #[test]
    fn forward_with_exhausted_ttl_is_rejected_with_hop_limit() {
        let handler = RoutingHandler { owns: false };
        let req = RoutingReq(StripeKey([0; 32]));

        match first_outcome(&handler, &req, 0) {
            FirstOutcome::Rejected(msg) => {
                assert!(
                    msg.contains("hop limit"),
                    "expected hop-limit rejection, got {msg:?}",
                );
            }
            other => panic!("forward at ttl == 0 must be rejected, got {other:?}"),
        }
    }

    #[test]
    fn forward_with_remaining_ttl_still_serves() {
        let handler = RoutingHandler { owns: false };
        let req = RoutingReq(StripeKey([0; 32]));

        match first_outcome(&handler, &req, 1) {
            FirstOutcome::Served(_) => {}
            other => panic!("forward with budget must serve, got {other:?}"),
        }
    }
}
