// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Client-side `bufferpool::Transport<R>` implementation over the
//! fabric module. One transport per NUMA shard.
//!
//! Wire protocol (mirror of the server-side responder in `rpc.rs`):
//!
//! 1. Client allocates a `request_id` (32 bits), serializes a
//!    [`super::rpc::RequestHeader`] plus the bincode-encoded `R`
//!    into a single buffer, and `fi_tsend`s it with
//!    `tag = REQUEST_TAG_BASE | request_id` to the routed peer.
//! 2. Client posts one recv per expected destination page tagged
//!    `PAGE_ACK_TAG_BASE | request_id`, plus separate recvs for
//!    `RESPONSE_END_TAG_BASE | request_id` and
//!    `ERROR_ACK_TAG_BASE | request_id`. Launch rejects requests that
//!    cannot fit within the completion registry before posting any recv.
//! 3. For each `PAGE_ACK`, the stream yields
//!    `dsts[page_idx_from_payload]`.
//! 4. `RESPONSE_END` resolves the stream with `None`; `ERROR_ACK`
//!    yields `Some(Err(_))` and then `None`.
//!
//! Drop: outstanding recv contexts are `fi_cancel`led so libfabric
//! reclaims its references promptly. The client does not currently
//! signal the server about mid-stream cancellation - the server
//! noticing falls out of the same fabric tear-down at the end of a
//! test (Phase 5b will reconsider once the wire is exercised under
//! real workloads).
//!
//! **MR strategy**: page data lands directly in the caller-provided
//! `MrHandle` (the buffer pool's registered backing) via server-side
//! `fi_write`; the transport never copies page bytes through its own
//! buffers. Per-operation control buffers (request header+body send,
//! `PAGE_ACK`/`RESPONSE_END`/`ERROR_ACK` recvs) are heap-allocated
//! `Box<[u8; N]>`s sized for the wire payload and freed by the
//! completion handler. There is no shared bounce-buffer MR; the
//! provider-required local descriptors are satisfied with
//! `desc=NULL` (mirroring `ping.rs`).

use std::collections::VecDeque;
use std::marker::PhantomData;
use std::pin::Pin;
use std::ptr;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use serde::Serialize;
use serde::de::DeserializeOwned;

use crate::bufferpool::{BulkRef, Error as PoolError, PageRef, PageStream, PeerId, Req, Transport};

use super::backing::MrHandle;
use super::completion::{CompletionInfo, CompletionSlot};
use super::error::{FabricError, Result as FabResult};
use super::fabric::Fabric;
use super::ffi;
use super::rpc::{
    ERROR_ACK_TAG_BASE, ErrorAck, PAGE_ACK_TAG_BASE, PageAck, REQUEST_TAG_BASE,
    RESPONSE_END_TAG_BASE, RequestHeader,
};

/// Selects a peer for a given request. Returns `Option` so the
/// no-peer case (empty routing table) is non-fatal at construction
/// time; the stream surfaces it as an error on first poll instead.
pub trait PeerRouter<R>: Send + Sync + 'static {
    fn route(&self, req: &R) -> Option<PeerId>;
}

/// Constant router: every request routes to `peer`.
pub struct StaticPeer {
    pub peer: PeerId,
}

impl<R> PeerRouter<R> for StaticPeer {
    fn route(&self, _req: &R) -> Option<PeerId> {
        Some(self.peer)
    }
}

/// Client-side fabric transport. Carries the MR the server will
/// `fi_write` destination pages into.
pub struct FabricTransport<R, P> {
    fabric: Arc<Fabric>,
    mr: MrHandle,
    router: P,
    next_request_id: AtomicU32,
    _marker: PhantomData<fn() -> R>,
}

impl<R, P> FabricTransport<R, P> {
    /// Build a transport bound to `fabric` with destination MR `mr`.
    /// `page_size` must divide `mr.len`; it is used only as a
    /// construction-time sanity check that the caller's MR geometry
    /// matches the pool's. The server-side wire planner still derives
    /// page geometry from a crate-level constant - see the TODO in
    /// `fabric/rpc.rs::plan_page_write`.
    pub fn new(fabric: Arc<Fabric>, mr: MrHandle, router: P, page_size: usize) -> FabResult<Self> {
        if page_size == 0 {
            return Err(FabricError::BadConfig("page_size must be > 0"));
        }
        if mr.len % page_size != 0 {
            return Err(FabricError::BadConfig(
                "mr.len must be a multiple of page_size",
            ));
        }
        Ok(Self {
            fabric,
            mr,
            router,
            next_request_id: AtomicU32::new(1),
            _marker: PhantomData,
        })
    }
}

impl<R, P> Transport<R> for FabricTransport<R, P>
where
    R: Req + Serialize + DeserializeOwned + 'static,
    P: PeerRouter<R>,
{
    type Stream<'a>
        = FabricBulkStream<'a, R>
    where
        Self: 'a,
        R: 'a;

    fn bulk_get<'a>(&'a self, req: &'a R, src: BulkRef, dsts: &'a [PageRef]) -> Self::Stream<'a> {
        FabricBulkStream::new_unstarted(self, req, src, dsts)
    }
}

/// Shared state between the stream and the per-slot completion
/// handlers running on the progress thread.
pub(crate) struct StreamShared {
    /// FIFO of recv results arriving from completion handlers.
    queue: Mutex<VecDeque<RecvCompletion>>,
    /// Waker registered by the stream when it last returned Pending.
    waker: Mutex<Option<Waker>>,
}

impl StreamShared {
    fn push(&self, item: RecvCompletion) {
        if let Ok(mut q) = self.queue.lock() {
            q.push_back(item);
        }
        if let Ok(mut w) = self.waker.lock() {
            if let Some(w) = w.take() {
                w.wake();
            }
        }
    }
}

struct RecvCompletion {
    op_context: usize,
    result: FabResult<RecvPayload>,
}

struct RecvPayload {
    tag: u64,
    payload: Vec<u8>,
}

/// Stream returned by `FabricTransport::bulk_get`. On first poll it
/// posts all recvs and the request send; subsequent polls drain
/// completions and yield pages.
pub struct FabricBulkStream<'a, R> {
    state: StreamState<'a>,
    _marker: PhantomData<fn() -> R>,
}

enum StreamState<'a> {
    /// Pre-launch failure captured during stream construction. First
    /// poll returns it without touching libfabric.
    Failed {
        error: Option<FabricError>,
    },
    /// Pre-launch state: nothing posted yet. We hold &Transport so
    /// the first poll can drive submission.
    Pending {
        fabric: Arc<Fabric>,
        mr: MrHandle,
        peer: PeerId,
        request_id: u32,
        src: BulkRef,
        req_bytes: Vec<u8>,
        dsts: &'a [PageRef],
    },
    /// Active state: recvs posted, awaiting acks.
    Active {
        shared: Arc<StreamShared>,
        dsts: &'a [PageRef],
        /// Raw recv contexts we may need to cancel on drop. Each is
        /// a `*mut CompletionSlot` we minted via `into_raw`. When a
        /// completion arrives the handler removes its entry.
        recv_ctxs: Mutex<Vec<*mut std::ffi::c_void>>,
        /// Allocated recv buffers (Box leaks owned by handlers).
        ep: *mut ffi::fid_ep,
        acked: Vec<bool>,
        ended: bool,
        emitted_error: bool,
    },
    Done,
}

// SAFETY: We hold raw pointers to libfabric resources that are
// thread-safe per its docs; the contexts in `recv_ctxs` are only
// touched by the stream's own Drop and by the progress thread.
unsafe impl<'a, R> Send for FabricBulkStream<'a, R> {}

impl<'a, R> FabricBulkStream<'a, R>
where
    R: Req + Serialize,
{
    fn new_unstarted<P>(
        transport: &FabricTransport<R, P>,
        req: &'a R,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self
    where
        P: PeerRouter<R>,
    {
        // Allocate request_id eagerly so it's stable for the stream's
        // lifetime. Error materialization waits until first poll so
        // the trait signature stays infallible.
        let request_id = transport.next_request_id.fetch_add(1, Ordering::Relaxed);
        let (peer, req_bytes) = match route_and_serialize(&transport.router, req) {
            Ok(v) => v,
            Err(error) => return Self::failed(error),
        };

        Self {
            state: StreamState::Pending {
                fabric: transport.fabric.clone(),
                mr: transport.mr,
                peer,
                request_id,
                src,
                req_bytes,
                dsts,
            },
            _marker: PhantomData,
        }
    }

    fn failed(error: FabricError) -> Self {
        Self {
            state: StreamState::Failed { error: Some(error) },
            _marker: PhantomData,
        }
    }
}

impl<'a, R> PageStream for FabricBulkStream<'a, R> {
    fn poll_next(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, PoolError>>> {
        // SAFETY: standard pin projection through `&mut self`.
        let this = unsafe { self.as_mut().get_unchecked_mut() };
        loop {
            match &mut this.state {
                StreamState::Done => return Poll::Ready(None),
                StreamState::Failed { error } => {
                    let error = error.take().expect("pre-launch error already consumed");
                    this.state = StreamState::Done;
                    return Poll::Ready(Some(Err(PoolError::transport(error))));
                }
                StreamState::Pending { .. } => {
                    // Transition Pending -> Active.
                    let prev = std::mem::replace(&mut this.state, StreamState::Done);
                    let (fabric, mr, peer, request_id, src, req_bytes, dsts) = match prev {
                        StreamState::Pending {
                            fabric,
                            mr,
                            peer,
                            request_id,
                            src,
                            req_bytes,
                            dsts,
                        } => (fabric, mr, peer, request_id, src, req_bytes, dsts),
                        _ => unreachable!(),
                    };
                    match launch(&fabric, &mr, peer, request_id, src, &req_bytes, dsts) {
                        Ok((shared, ep, recv_ctxs)) => {
                            this.state = StreamState::Active {
                                shared,
                                dsts,
                                recv_ctxs: Mutex::new(recv_ctxs),
                                ep,
                                acked: vec![false; dsts.len()],
                                ended: false,
                                emitted_error: false,
                            };
                            // Fall through to poll the Active state.
                        }
                        Err(e) => {
                            return Poll::Ready(Some(Err(PoolError::transport(e))));
                        }
                    }
                }
                StreamState::Active {
                    shared,
                    dsts,
                    recv_ctxs,
                    ep: _,
                    acked,
                    ended,
                    emitted_error,
                } => {
                    if *ended {
                        return Poll::Ready(None);
                    }
                    // Register waker first to avoid a lost wakeup.
                    if let Ok(mut w) = shared.waker.lock() {
                        *w = Some(cx.waker().clone());
                    }
                    let item = shared.queue.lock().ok().and_then(|mut q| q.pop_front());
                    match item {
                        None => return Poll::Pending,
                        Some(item) => {
                            if let Ok(mut ctxs) = recv_ctxs.lock() {
                                remove_recv_context(&mut ctxs, item.op_context);
                            }
                            let RecvCompletion { result, .. } = item;
                            let RecvPayload { tag, payload } = match result {
                                Ok(payload) => payload,
                                Err(e) => {
                                    *ended = true;
                                    return Poll::Ready(Some(Err(PoolError::transport(e))));
                                }
                            };
                            let tag_base = tag & 0xFFFF_FFFF_0000_0000u64;
                            if tag_base == PAGE_ACK_TAG_BASE {
                                let ack: PageAck = match bincode::deserialize(&payload) {
                                    Ok(v) => v,
                                    Err(_) => {
                                        *ended = true;
                                        return Poll::Ready(Some(Err(PoolError::transport(
                                            FabricError::BadConfig("malformed PAGE_ACK"),
                                        ))));
                                    }
                                };
                                let idx = ack.page_idx as usize;
                                if idx >= dsts.len() {
                                    *ended = true;
                                    return Poll::Ready(Some(Err(PoolError::transport(
                                        FabricError::BadConfig("PAGE_ACK index out of range"),
                                    ))));
                                }
                                if acked[idx] {
                                    *ended = true;
                                    return Poll::Ready(Some(Err(PoolError::transport(
                                        FabricError::BadConfig("duplicate PAGE_ACK index"),
                                    ))));
                                }
                                acked[idx] = true;
                                return Poll::Ready(Some(Ok(dsts[idx])));
                            } else if tag_base == RESPONSE_END_TAG_BASE {
                                *ended = true;
                                if acked.iter().any(|acked| !*acked) {
                                    return Poll::Ready(Some(Err(PoolError::transport(
                                        FabricError::BadConfig(
                                            "RESPONSE_END before all requested pages were delivered",
                                        ),
                                    ))));
                                }
                                return Poll::Ready(None);
                            } else if tag_base == ERROR_ACK_TAG_BASE {
                                *ended = true;
                                if *emitted_error {
                                    return Poll::Ready(None);
                                }
                                *emitted_error = true;
                                let msg = bincode::deserialize::<ErrorAck>(&payload)
                                    .map(|e| e.message)
                                    .unwrap_or_else(|_| "server error (undecodable)".into());
                                return Poll::Ready(Some(Err(PoolError::transport(ServerError(
                                    msg,
                                )))));
                            } else {
                                // Stray completion; drop and continue.
                                continue;
                            }
                        }
                    }
                }
            }
        }
    }
}

fn route_and_serialize<R, P>(router: &P, req: &R) -> FabResult<(PeerId, Vec<u8>)>
where
    R: Req + Serialize,
    P: PeerRouter<R>,
{
    let peer = router
        .route(req)
        .ok_or(FabricError::NotFound("peer route"))?;
    let req_bytes = bincode::serialize(req).map_err(|e| {
        FabricError::Encode(Arc::new(std::io::Error::new(
            std::io::ErrorKind::Other,
            format!("request encode: {e}"),
        )))
    })?;
    Ok((peer, req_bytes))
}

impl<'a, R> Drop for FabricBulkStream<'a, R> {
    fn drop(&mut self) {
        if let StreamState::Active { recv_ctxs, ep, .. } = &mut self.state {
            if let Ok(ctxs) = recv_ctxs.lock() {
                for ctx in ctxs.iter() {
                    // Best-effort cancel. Outstanding completions will
                    // surface as cancel errors on the CQ; their handlers
                    // free their buffers/slots.
                    // SAFETY: `ep` is owned by the live fabric; `*ctx`
                    // is the same context we passed to `fi_trecv`.
                    unsafe {
                        let _ = ffi::ub_fi_cancel(ffi::as_fid_ep(*ep), *ctx);
                    }
                }
            }
        }
    }
}

/// Server-side error materialized through `ERROR_ACK`. Carries the
/// stringified message the handler emitted.
#[derive(Debug)]
pub struct ServerError(pub String);

impl std::fmt::Display for ServerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "fabric server error: {}", self.0)
    }
}

impl std::error::Error for ServerError {}

/// Submit the request and post the recvs. Returns the shared state
/// plus the EP pointer and contexts the stream Drop must cancel if
/// they remain outstanding.
fn launch(
    fabric: &Arc<Fabric>,
    mr: &MrHandle,
    peer: PeerId,
    request_id: u32,
    src: BulkRef,
    req_bytes: &[u8],
    dsts: &[PageRef],
) -> FabResult<(
    Arc<StreamShared>,
    *mut ffi::fid_ep,
    Vec<*mut std::ffi::c_void>,
)> {
    ensure_launch_fits_registry(fabric.completions().available_count(), dsts.len())?;

    let fi_addr = fabric.lookup_fi_addr(peer)?;
    let inner = fabric.inner();
    let ep = inner.ep();

    let shared = Arc::new(StreamShared {
        queue: Mutex::new(VecDeque::new()),
        waker: Mutex::new(None),
    });

    let mut recv_guard = PostedRecvGuard::new(ep, expected_recv_count(dsts.len()).unwrap_or(0));

    // Post one PAGE_ACK recv per expected destination page.
    let page_ack_tag = PAGE_ACK_TAG_BASE | (request_id as u64);
    for _ in 0..dsts.len() {
        let (slot, _fut) = fabric.completions().allocate()?;
        let buf: Box<[u8; 64]> = Box::new([0u8; 64]);
        let buf_ptr = Box::into_raw(buf);
        let ctx_addr = (&*slot as *const CompletionSlot) as usize;
        let shared_for_handler = shared.clone();
        let buf_addr = buf_ptr as usize;
        slot.set_handler(move |result| {
            // SAFETY: buf_addr was just produced by Box::into_raw.
            let recv_buf: Box<[u8; 64]> = unsafe { Box::from_raw(buf_addr as *mut [u8; 64]) };
            shared_for_handler.push(recv_completion(result, ctx_addr, &recv_buf));
        });
        let ctx = slot.into_raw();
        // SAFETY: ep, buffer, ctx all live for the call. The recv
        // matches any low-32-bit value but we asked for the exact
        // tag - libfabric will still match against fi_addr only when
        // src_addr != FI_ADDR_UNSPEC. Here we accept from anyone.
        let rc = unsafe {
            ffi::ub_fi_trecv(
                ep,
                buf_ptr as *mut std::ffi::c_void,
                64,
                ptr::null_mut(),
                ffi::FI_ADDR_UNSPEC,
                page_ack_tag,
                0,
                ctx,
            )
        };
        if rc < 0 {
            // Reclaim the current unposted operation. The guard
            // cancels any earlier recvs before launch returns.
            // SAFETY: ctx was just produced from into_raw.
            let _ = unsafe { CompletionSlot::from_raw(ctx) };
            // SAFETY: same for the buffer.
            let _ = unsafe { Box::from_raw(buf_ptr) };
            return Err(FabricError::Pkg("fi_trecv (page_ack)", rc as i32));
        }
        recv_guard.push(ctx);
    }

    // Post a RESPONSE_END recv.
    let end_tag = RESPONSE_END_TAG_BASE | (request_id as u64);
    {
        let (slot, _fut) = fabric.completions().allocate()?;
        let buf: Box<[u8; 512]> = Box::new([0u8; 512]);
        let buf_ptr = Box::into_raw(buf);
        let ctx_addr = (&*slot as *const CompletionSlot) as usize;
        let shared_for_handler = shared.clone();
        let buf_addr = buf_ptr as usize;
        slot.set_handler(move |result| {
            // SAFETY: buf_addr was just produced by Box::into_raw.
            let recv_buf: Box<[u8; 512]> = unsafe { Box::from_raw(buf_addr as *mut [u8; 512]) };
            shared_for_handler.push(recv_completion(result, ctx_addr, &recv_buf));
        });
        let ctx = slot.into_raw();
        let rc = unsafe {
            ffi::ub_fi_trecv(
                ep,
                buf_ptr as *mut std::ffi::c_void,
                512,
                ptr::null_mut(),
                ffi::FI_ADDR_UNSPEC,
                end_tag,
                0,
                ctx,
            )
        };
        if rc < 0 {
            // SAFETY: ctx just produced.
            let _ = unsafe { CompletionSlot::from_raw(ctx) };
            let _ = unsafe { Box::from_raw(buf_ptr) };
            return Err(FabricError::Pkg("fi_trecv (resp_end)", rc as i32));
        }
        recv_guard.push(ctx);
    }
    // Also post an ERROR_ACK recv.
    let err_tag = ERROR_ACK_TAG_BASE | (request_id as u64);
    {
        let (slot, _fut) = fabric.completions().allocate()?;
        let buf: Box<[u8; 4096]> = Box::new([0u8; 4096]);
        let buf_ptr = Box::into_raw(buf);
        let ctx_addr = (&*slot as *const CompletionSlot) as usize;
        let shared_for_handler = shared.clone();
        let buf_addr = buf_ptr as usize;
        slot.set_handler(move |result| {
            // SAFETY: buf_addr was just produced by Box::into_raw.
            let recv_buf: Box<[u8; 4096]> = unsafe { Box::from_raw(buf_addr as *mut [u8; 4096]) };
            shared_for_handler.push(recv_completion(result, ctx_addr, &recv_buf));
        });
        let ctx = slot.into_raw();
        let rc = unsafe {
            ffi::ub_fi_trecv(
                ep,
                buf_ptr as *mut std::ffi::c_void,
                4096,
                ptr::null_mut(),
                ffi::FI_ADDR_UNSPEC,
                err_tag,
                0,
                ctx,
            )
        };
        if rc < 0 {
            // SAFETY: ctx just produced.
            let _ = unsafe { CompletionSlot::from_raw(ctx) };
            let _ = unsafe { Box::from_raw(buf_ptr) };
            return Err(FabricError::Pkg("fi_trecv (err_ack)", rc as i32));
        }
        recv_guard.push(ctx);
    }

    // Build and send the request: header followed by the
    // bincode-serialized req body.
    let header = RequestHeader::new(request_id, mr, dsts.len() as u32, src);
    let mut buf: Vec<u8> = bincode::serialize(&header).map_err(|e| {
        FabricError::Encode(Arc::new(std::io::Error::new(
            std::io::ErrorKind::Other,
            format!("header encode: {e}"),
        )))
    })?;
    buf.extend_from_slice(req_bytes);

    let (send_slot, _send_fut) = fabric.completions().allocate()?;
    let send_len = buf.len();
    let send_box: Box<[u8]> = buf.into_boxed_slice();
    let send_buf_ptr = Box::into_raw(send_box);
    let buf_addr = send_buf_ptr as *mut u8 as usize;
    let buf_len = send_len;
    send_slot.set_handler(move |_| {
        // SAFETY: buf_addr/buf_len were just produced by Box::into_raw
        // on a Box<[u8]>; libfabric returns ownership exactly once.
        let _ = unsafe {
            Box::from_raw(std::slice::from_raw_parts_mut(buf_addr as *mut u8, buf_len) as *mut [u8])
        };
    });
    let send_ctx = send_slot.into_raw();
    let send_tag = REQUEST_TAG_BASE | (request_id as u64);
    let rc = unsafe {
        ffi::ub_fi_tsend(
            ep,
            send_buf_ptr as *const std::ffi::c_void,
            send_len,
            ptr::null_mut(),
            fi_addr,
            send_tag,
            send_ctx,
        )
    };
    if rc < 0 {
        // SAFETY: just produced.
        let _ = unsafe { CompletionSlot::from_raw(send_ctx) };
        // SAFETY: just produced.
        let _ = unsafe {
            Box::from_raw(
                std::slice::from_raw_parts_mut(send_buf_ptr as *mut u8, send_len) as *mut [u8],
            )
        };
        return Err(FabricError::Pkg("fi_tsend (request)", rc as i32));
    }

    Ok((shared, ep, recv_guard.into_contexts()))
}

struct PostedRecvGuard {
    ep: *mut ffi::fid_ep,
    ctxs: Vec<*mut std::ffi::c_void>,
    armed: bool,
}

impl PostedRecvGuard {
    fn new(ep: *mut ffi::fid_ep, capacity: usize) -> Self {
        Self {
            ep,
            ctxs: Vec::with_capacity(capacity),
            armed: true,
        }
    }

    fn push(&mut self, ctx: *mut std::ffi::c_void) {
        self.ctxs.push(ctx);
    }

    fn into_contexts(mut self) -> Vec<*mut std::ffi::c_void> {
        self.armed = false;
        std::mem::take(&mut self.ctxs)
    }
}

impl Drop for PostedRecvGuard {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        for ctx in self.ctxs.iter() {
            // Best-effort cancel; the completion handler reclaims the
            // buffer and slot when the provider reports cancellation.
            // SAFETY: `ep` is owned by the live fabric and each `ctx`
            // was accepted by `fi_trecv` before it was pushed here.
            unsafe {
                let _ = ffi::ub_fi_cancel(ffi::as_fid_ep(self.ep), *ctx);
            }
        }
    }
}

#[doc(hidden)]
pub fn ensure_launch_fits_registry(available_slots: usize, dst_count: usize) -> FabResult<()> {
    match required_completion_slots(dst_count) {
        Some(required) if required <= available_slots => Ok(()),
        _ => Err(FabricError::BadConfig(
            "bulk_get request exceeds available completion slots",
        )),
    }
}

#[doc(hidden)]
pub fn required_completion_slots(dst_count: usize) -> Option<usize> {
    expected_recv_count(dst_count)?.checked_add(1)
}

fn expected_recv_count(dst_count: usize) -> Option<usize> {
    dst_count.checked_add(2)
}

fn recv_completion<const N: usize>(
    result: &FabResult<CompletionInfo>,
    op_context: usize,
    recv_buf: &[u8; N],
) -> RecvCompletion {
    match result {
        Ok(info) => {
            let len = info.bytes.min(recv_buf.len());
            RecvCompletion {
                op_context: info.op_context,
                result: Ok(RecvPayload {
                    tag: info.tag,
                    payload: recv_buf[..len].to_vec(),
                }),
            }
        }
        Err(e) => RecvCompletion {
            op_context,
            result: Err(e.clone()),
        },
    }
}

fn remove_recv_context(ctxs: &mut Vec<*mut std::ffi::c_void>, completed: usize) -> bool {
    if let Some(pos) = ctxs.iter().position(|ctx| *ctx as usize == completed) {
        ctxs.swap_remove(pos);
        true
    } else {
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde::ser::Serializer;
    use std::task::{RawWaker, RawWakerVTable};

    struct NoRoute;

    impl<R> PeerRouter<R> for NoRoute {
        fn route(&self, _req: &R) -> Option<PeerId> {
            None
        }
    }

    struct TestRouter;

    impl<R> PeerRouter<R> for TestRouter {
        fn route(&self, _req: &R) -> Option<PeerId> {
            Some(PeerId(7))
        }
    }

    #[derive(Serialize)]
    struct GoodReq;

    impl Req for GoodReq {
        fn key(&self) -> crate::bufferpool::StripeKey {
            crate::bufferpool::StripeKey([0; 32])
        }
    }

    struct BadSerializeReq;

    impl Req for BadSerializeReq {
        fn key(&self) -> crate::bufferpool::StripeKey {
            crate::bufferpool::StripeKey([0; 32])
        }
    }

    impl Serialize for BadSerializeReq {
        fn serialize<S>(&self, _serializer: S) -> Result<S::Ok, S::Error>
        where
            S: Serializer,
        {
            Err(serde::ser::Error::custom("forced serialize failure"))
        }
    }

    fn active_stream<'a>(dsts: &'a [PageRef], acked: Vec<bool>) -> FabricBulkStream<'a, ()> {
        active_stream_with_contexts(dsts, acked, Vec::new())
    }

    fn active_stream_with_contexts<'a>(
        dsts: &'a [PageRef],
        acked: Vec<bool>,
        recv_ctxs: Vec<*mut std::ffi::c_void>,
    ) -> FabricBulkStream<'a, ()> {
        assert_eq!(acked.len(), dsts.len());
        FabricBulkStream {
            state: StreamState::Active {
                shared: Arc::new(StreamShared {
                    queue: Mutex::new(VecDeque::new()),
                    waker: Mutex::new(None),
                }),
                dsts,
                recv_ctxs: Mutex::new(recv_ctxs),
                ep: ptr::null_mut(),
                acked,
                ended: false,
                emitted_error: false,
            },
            _marker: PhantomData,
        }
    }

    fn push_page_ack(stream: &mut FabricBulkStream<'_, ()>, page_idx: u32) {
        let StreamState::Active { shared, .. } = &stream.state else {
            panic!("expected active stream");
        };
        let payload = bincode::serialize(&PageAck { page_idx }).expect("serialize page ack");
        shared.push(RecvCompletion {
            op_context: 0,
            result: Ok(RecvPayload {
                tag: PAGE_ACK_TAG_BASE,
                payload,
            }),
        });
    }

    fn push_recv_error(stream: &mut FabricBulkStream<'_, ()>, op_context: usize) {
        let StreamState::Active { shared, .. } = &stream.state else {
            panic!("expected active stream");
        };
        shared.push(RecvCompletion {
            op_context,
            result: Err(FabricError::Cq {
                prov_errno: -3,
                err: -5,
            }),
        });
    }

    fn page_ack_info(payload_len: usize) -> CompletionInfo {
        CompletionInfo {
            flags: 0,
            bytes: payload_len,
            tag: PAGE_ACK_TAG_BASE,
            src_addr: 0,
            op_context: 0,
        }
    }

    fn push_response_end(stream: &mut FabricBulkStream<'_, ()>) {
        let StreamState::Active { shared, .. } = &stream.state else {
            panic!("expected active stream");
        };
        shared.push(RecvCompletion {
            op_context: 0,
            result: Ok(RecvPayload {
                tag: RESPONSE_END_TAG_BASE,
                payload: Vec::new(),
            }),
        });
    }

    fn noop_waker() -> std::task::Waker {
        fn no(_: *const ()) {}
        fn clone(_: *const ()) -> RawWaker {
            RawWaker::new(ptr::null(), &VT)
        }
        static VT: RawWakerVTable = RawWakerVTable::new(clone, no, no, no);
        // SAFETY: vtable never dereferences the data pointer.
        unsafe { std::task::Waker::from_raw(RawWaker::new(ptr::null(), &VT)) }
    }

    fn poll_once<R>(
        stream: &mut FabricBulkStream<'_, R>,
    ) -> Poll<Option<Result<PageRef, PoolError>>> {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        // SAFETY: stream is pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(stream) };
        stream.as_mut().poll_next(&mut cx)
    }

    #[test]
    fn no_route_returns_error_without_defaulting_to_peer_zero() {
        let err = route_and_serialize(&NoRoute, &GoodReq).unwrap_err();

        assert!(matches!(err, FabricError::NotFound("peer route")));
        let mut stream = FabricBulkStream::<GoodReq>::failed(err);
        match poll_once(&mut stream) {
            Poll::Ready(Some(Err(PoolError::Transport(err)))) => {
                assert!(
                    err.to_string().contains("peer route"),
                    "unexpected error: {err}",
                );
            }
            other => panic!("expected transport error, got {other:?}"),
        }
        assert!(matches!(poll_once(&mut stream), Poll::Ready(None)));
    }

    #[test]
    fn request_serialization_failure_returns_error_without_empty_body() {
        let err = route_and_serialize(&TestRouter, &BadSerializeReq).unwrap_err();

        assert!(matches!(err, FabricError::Encode(_)));
        assert!(
            err.to_string().contains("forced serialize failure"),
            "unexpected error: {err}",
        );
        let mut stream = FabricBulkStream::<BadSerializeReq>::failed(err);
        match poll_once(&mut stream) {
            Poll::Ready(Some(Err(PoolError::Transport(err)))) => {
                assert!(
                    err.to_string().contains("forced serialize failure"),
                    "unexpected error: {err}",
                );
            }
            other => panic!("expected transport error, got {other:?}"),
        }
        assert!(matches!(poll_once(&mut stream), Poll::Ready(None)));
    }

    #[test]
    fn recv_completion_preserves_success_payload() {
        let payload = bincode::serialize(&PageAck { page_idx: 7 }).expect("serialize page ack");
        let mut recv_buf = [0u8; 64];
        recv_buf[..payload.len()].copy_from_slice(&payload);
        let info = page_ack_info(payload.len());

        let completion = recv_completion(&Ok(info), 0x1234, &recv_buf);

        assert_eq!(completion.op_context, 0);
        match completion.result {
            Ok(payload) => {
                assert_eq!(payload.tag, PAGE_ACK_TAG_BASE);
                let ack: PageAck = bincode::deserialize(&payload.payload).expect("page ack");
                assert_eq!(ack.page_idx, 7);
            }
            Err(err) => panic!("unexpected recv error: {err}"),
        }
    }

    #[test]
    fn recv_completion_preserves_error_context() {
        let completion = recv_completion(
            &Err(FabricError::Cq {
                prov_errno: -3,
                err: -5,
            }),
            0x1234,
            &[0u8; 64],
        );

        assert_eq!(completion.op_context, 0x1234);
        assert!(matches!(
            completion.result,
            Err(FabricError::Cq {
                prov_errno: -3,
                err: -5
            })
        ));
    }

    #[test]
    fn required_completion_slots_counts_page_end_error_and_send() {
        assert_eq!(required_completion_slots(0), Some(3));
        assert_eq!(required_completion_slots(1), Some(4));
        assert_eq!(required_completion_slots(128), Some(131));
    }

    #[test]
    fn launch_available_slot_preflight_accepts_boundary() {
        assert!(ensure_launch_fits_registry(3, 0).is_ok());
        assert!(ensure_launch_fits_registry(4, 1).is_ok());
    }

    #[test]
    fn launch_available_slot_preflight_rejects_oversized_request() {
        let err = ensure_launch_fits_registry(3, 1).unwrap_err();
        assert!(matches!(
            err,
            FabricError::BadConfig("bulk_get request exceeds available completion slots")
        ));
    }

    #[test]
    fn launch_available_slot_preflight_rejects_overflow() {
        let err = ensure_launch_fits_registry(usize::MAX, usize::MAX).unwrap_err();
        assert!(matches!(
            err,
            FabricError::BadConfig("bulk_get request exceeds available completion slots")
        ));
    }

    #[test]
    fn response_end_before_all_pages_returns_transport_error() {
        let dsts = [
            PageRef {
                page_idx: 0,
                offset: 0,
                len: 4096,
            },
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4096,
            },
        ];
        let mut stream = active_stream(&dsts, vec![false, false]);
        push_response_end(&mut stream);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        // SAFETY: stream is pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        match stream.as_mut().poll_next(&mut cx) {
            Poll::Ready(Some(Err(PoolError::Transport(err)))) => {
                assert!(
                    err.to_string()
                        .contains("RESPONSE_END before all requested pages were delivered"),
                    "unexpected error: {err}",
                );
            }
            other => panic!("expected transport error, got {other:?}"),
        }
    }

    #[test]
    fn recv_error_returns_transport_error_and_removes_context_once() {
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        }];
        let ctx = 0x20usize as *mut std::ffi::c_void;
        let mut stream = active_stream_with_contexts(&dsts, vec![false], vec![ctx, ctx]);
        push_recv_error(&mut stream, ctx as usize);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        // SAFETY: stream is pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        match stream.as_mut().poll_next(&mut cx) {
            Poll::Ready(Some(Err(PoolError::Transport(err)))) => {
                assert!(
                    err.to_string().contains("libfabric cq error"),
                    "unexpected error: {err}",
                );
            }
            other => panic!("expected transport error, got {other:?}"),
        }
        {
            let StreamState::Active {
                recv_ctxs, ended, ..
            } = &stream.state
            else {
                panic!("expected active stream");
            };
            assert!(*ended);
            let ctxs = recv_ctxs.lock().expect("recv ctxs");
            assert_eq!(ctxs.len(), 1);
            assert_eq!(ctxs[0], ctx);
        }

        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(None)
        ));
        let StreamState::Active { recv_ctxs, .. } = &stream.state else {
            panic!("expected active stream");
        };
        let ctxs = recv_ctxs.lock().expect("recv ctxs");
        assert_eq!(ctxs.len(), 1);
        assert_eq!(ctxs[0], ctx);
        drop(ctxs);
        recv_ctxs.lock().expect("recv ctxs").clear();
    }

    #[test]
    fn duplicate_page_ack_returns_transport_error() {
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        }];
        let mut stream = active_stream(&dsts, vec![false]);
        push_page_ack(&mut stream, 0);
        push_page_ack(&mut stream, 0);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        // SAFETY: stream is pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(Some(Ok(PageRef { page_idx: 0, .. })))
        ));
        match stream.as_mut().poll_next(&mut cx) {
            Poll::Ready(Some(Err(PoolError::Transport(err)))) => {
                assert!(
                    err.to_string().contains("duplicate PAGE_ACK index"),
                    "unexpected error: {err}",
                );
            }
            other => panic!("expected transport error, got {other:?}"),
        }
    }

    #[test]
    fn response_end_rejects_missing_ack_despite_matching_count() {
        let dsts = [
            PageRef {
                page_idx: 0,
                offset: 0,
                len: 4096,
            },
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4096,
            },
        ];
        let mut stream = active_stream(&dsts, vec![true, false]);
        push_response_end(&mut stream);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        // SAFETY: stream is pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        match stream.as_mut().poll_next(&mut cx) {
            Poll::Ready(Some(Err(PoolError::Transport(err)))) => {
                assert!(
                    err.to_string()
                        .contains("RESPONSE_END before all requested pages were delivered"),
                    "unexpected error: {err}",
                );
            }
            other => panic!("expected transport error, got {other:?}"),
        }
    }

    #[test]
    fn response_end_after_all_pages_returns_eof() {
        let dsts = [
            PageRef {
                page_idx: 0,
                offset: 0,
                len: 4096,
            },
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4096,
            },
        ];
        let mut stream = active_stream(&dsts, vec![true; dsts.len()]);
        push_response_end(&mut stream);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        // SAFETY: stream is pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(None)
        ));
    }

    #[test]
    fn remove_recv_context_removes_exactly_one_match() {
        let a = 0x10usize as *mut std::ffi::c_void;
        let b = 0x20usize as *mut std::ffi::c_void;
        let c = 0x30usize as *mut std::ffi::c_void;
        let mut ctxs = vec![a, b, c, b];

        assert!(remove_recv_context(&mut ctxs, b as usize));

        assert_eq!(ctxs.len(), 3);
        assert_eq!(ctxs.iter().filter(|ctx| **ctx == b).count(), 1);
        assert!(ctxs.contains(&a));
        assert!(ctxs.contains(&c));
    }

    #[test]
    fn remove_recv_context_leaves_missing_contexts_unchanged() {
        let a = 0x10usize as *mut std::ffi::c_void;
        let b = 0x20usize as *mut std::ffi::c_void;
        let missing = 0x30usize as *mut std::ffi::c_void;
        let mut ctxs = vec![a, b];

        assert!(!remove_recv_context(&mut ctxs, missing as usize));

        assert_eq!(ctxs, vec![a, b]);
    }
}
