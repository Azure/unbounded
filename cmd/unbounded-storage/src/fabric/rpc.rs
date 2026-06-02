// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Server-side fabric RPC: receive a request, drive a `Handler`
//! stream, RMA-write pages to the client's destination MR, and
//! ack each page.
//!
//! Wire framing (mirror of `transport.rs`):
//!
//! * Reserved tag bases ([`REQUEST_TAG_BASE`], [`PAGE_ACK_TAG_BASE`],
//!   [`RESPONSE_END_TAG_BASE`], [`ERROR_ACK_TAG_BASE`]). The low 32
//!   bits of every tag carry the `request_id`.
//! * On the wire: client `fi_tsend`s a bincode-encoded
//!   [`RequestHeader`] followed by the bincode-encoded `R` body.
//! * Server replies with one `fi_tsend(PAGE_ACK_TAG_BASE | rid,
//!   bincode(PageAck))` per RMA-written page, then either
//!   `fi_tsend(RESPONSE_END_TAG_BASE | rid)` (success, zero payload)
//!   or `fi_tsend(ERROR_ACK_TAG_BASE | rid, bincode(ErrorAck))`.
//!
//! **Worker model**: each in-flight request is processed by a
//! dedicated OS thread spawned via the fabric's runtime. The thread
//! drives the handler stream synchronously, polling it with a
//! noop-waker; it then submits libfabric ops and blocks on their
//! `CompletionFuture`s via a spin-poller (mirroring `ping.rs`).
//! `RpcServerHandle::drop` sets a shutdown flag that workers check
//! between handler-stream polls; this is the mechanism that drops
//! mid-stream handlers when the embedder tears the server down.
//!
//! **MR strategy** (per Phase 5a spec, option (a)): we allocate
//! request-recv buffers per outstanding recv and free them in the
//! recv completion handler. No bounce-buffer MR is registered for
//! ack sends - the tcp / verbs providers' FI_MR_LOCAL requirement is
//! satisfied with `desc=NULL` in practice, mirroring `ping.rs`.

use std::ptr;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::time::Duration;

use serde::Serialize;
use serde::de::DeserializeOwned;

use crate::bufferpool::{BulkRef, Req, StripeKey};
use crate::runtime::WorkerIdx;

use super::backing::MrHandle;
use super::completion::{CompletionFuture, CompletionInfo, CompletionSlot};
use super::error::{FabricError, Result as FabResult};
use super::fabric::{Fabric, FabricInner};
use super::ffi;
use super::handler::{Handler, HandlerStream};

// Reserved tag bases for the RPC protocol. The low 32 bits carry the
// request id; the high 32 bits select the message class.
pub const REQUEST_TAG_BASE: u64 = 0xFFFF_FFFC_0000_0000;
pub const PAGE_ACK_TAG_BASE: u64 = 0xFFFF_FFFA_0000_0000;
pub const RESPONSE_END_TAG_BASE: u64 = 0xFFFF_FFF8_0000_0000;
pub const ERROR_ACK_TAG_BASE: u64 = 0xFFFF_FFF6_0000_0000;

const REQ_TAG_IGNORE: u64 = 0x0000_0000_FFFF_FFFF;
const REQUEST_RECV_BUF_LEN: usize = 64 * 1024;

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
        dest_mr: &MrHandle,
        dest_pages: u32,
        src: BulkRef,
        ttl: u32,
    ) -> Self {
        Self {
            request_id,
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

#[derive(serde::Serialize, serde::Deserialize)]
pub(crate) struct ErrorAck {
    pub message: String,
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
    shutdown: AtomicBool,
    inflight: AtomicU64,
}

/// Handle returned from `Fabric::start_rpc_server`. Drop signals
/// shutdown to outstanding workers and waits briefly for them to
/// observe the flag.
pub struct RpcServerHandle {
    shared: Arc<ServerShared>,
}

impl Drop for RpcServerHandle {
    fn drop(&mut self) {
        self.shared.shutdown.store(true, Ordering::Release);
        let started = std::time::Instant::now();
        while self.shared.inflight.load(Ordering::Acquire) > 0
            && started.elapsed() < Duration::from_secs(2)
        {
            std::thread::sleep(Duration::from_millis(5));
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
        H: Handler<R> + 'static,
    {
        let shared = Arc::new(ServerShared {
            fabric: self.inner_arc(),
            local_mr,
            page_size,
            shutdown: AtomicBool::new(false),
            inflight: AtomicU64::new(0),
        });
        let max_inflight = shared.fabric.cfg.max_inflight;
        let posted = (max_inflight / 8).max(1).min(4);
        for _ in 0..posted {
            post_request_recv::<R, H>(&shared, &handler)?;
        }
        Ok(RpcServerHandle { shared })
    }
}

fn post_request_recv<R, H>(shared: &Arc<ServerShared>, handler: &Arc<H>) -> FabResult<()>
where
    R: Req + Serialize + DeserializeOwned + Send + 'static,
    H: Handler<R> + 'static,
{
    let inner = &shared.fabric;
    let buf: Box<[u8; REQUEST_RECV_BUF_LEN]> = Box::new([0u8; REQUEST_RECV_BUF_LEN]);
    let buf_ptr = Box::into_raw(buf);
    let (slot, _fut) = inner.completions.allocate()?;

    let shared_for_handler = shared.clone();
    let handler_for_handler = handler.clone();
    let buf_addr = buf_ptr as usize;
    slot.set_handler(move |result| {
        // SAFETY: buf_addr just produced by Box::into_raw; handler is
        // invoked exactly once by the progress thread.
        let recv_buf: Box<[u8; REQUEST_RECV_BUF_LEN]> =
            unsafe { Box::from_raw(buf_addr as *mut [u8; REQUEST_RECV_BUF_LEN]) };
        let info = match result {
            Ok(i) => i.clone(),
            Err(_) => return,
        };
        let bytes = &recv_buf[..info.bytes.min(recv_buf.len())];

        // bincode encodes RequestHeader as a fixed-width prefix
        // because every field is a fixed-width integer; ask bincode
        // for that prefix length and split the buffer.
        let header_len = RequestHeader::encoded_len().unwrap_or(0);
        if bytes.len() < header_len {
            let _ = post_request_recv::<R, H>(&shared_for_handler, &handler_for_handler);
            return;
        }
        let header: RequestHeader = match bincode::deserialize(&bytes[..header_len]) {
            Ok(h) => h,
            Err(_) => {
                let _ = post_request_recv::<R, H>(&shared_for_handler, &handler_for_handler);
                return;
            }
        };
        let req: R = match bincode::deserialize(&bytes[header_len..]) {
            Ok(r) => r,
            Err(_) => {
                let _ = post_request_recv::<R, H>(&shared_for_handler, &handler_for_handler);
                return;
            }
        };

        let _ = post_request_recv::<R, H>(&shared_for_handler, &handler_for_handler);

        let shared_for_worker = shared_for_handler.clone();
        let handler_for_worker = handler_for_handler.clone();
        let src_addr = info.src_addr;
        shared_for_worker.inflight.fetch_add(1, Ordering::AcqRel);
        let shared_for_decr = shared_for_worker.clone();
        let runtime = shared_for_worker.fabric.cfg.runtime.clone();
        let _h = runtime.spawn_pinned(
            WorkerIdx(0),
            "fabric-rpc-worker",
            Box::new(move || {
                run_worker::<R, H>(shared_for_worker, handler_for_worker, header, req, src_addr);
                shared_for_decr.inflight.fetch_sub(1, Ordering::AcqRel);
            }),
        );
    });
    let ctx = slot.into_raw();
    // SAFETY: ep, buf_ptr, ctx all live for the call.
    let rc = unsafe {
        ffi::ub_fi_trecv(
            inner.ep(),
            buf_ptr as *mut std::ffi::c_void,
            REQUEST_RECV_BUF_LEN,
            ptr::null_mut(),
            ffi::FI_ADDR_UNSPEC,
            REQUEST_TAG_BASE,
            REQ_TAG_IGNORE,
            ctx,
        )
    };
    if rc < 0 {
        // SAFETY: just produced from into_raw.
        let _ = unsafe { CompletionSlot::from_raw(ctx) };
        // SAFETY: just produced from Box::into_raw.
        let _ = unsafe { Box::from_raw(buf_ptr) };
        return Err(FabricError::Pkg("fi_trecv (request)", rc as i32));
    }
    Ok(())
}

fn run_worker<R, H>(
    shared: Arc<ServerShared>,
    handler: Arc<H>,
    header: RequestHeader,
    req: R,
    src_addr: u64,
) where
    R: Req,
    H: Handler<R>,
{
    if src_addr == ffi::FI_ADDR_UNSPEC {
        return;
    }
    // The hop budget (`ttl`) is NOT rejected here. The RPC layer is
    // generic over the handler and does not run the Chord routing, so
    // it cannot tell an owner-serve from a forward. A node that owns
    // the requested stripe must serve locally even at `ttl == 0` (this
    // is reachable when a chain is exactly `MAX_HOPS` long: the last
    // forwarder, with `hops_remaining == 1`, forwards with `ttl - 1 ==
    // 0` to the owner). Enforcing the hop limit unconditionally here
    // silently shrank the budget by one and turned a valid owner-serve
    // into a hard error. The decision belongs to the handler: it is
    // handed `header.ttl` as `hops_remaining` below and rejects only
    // when it would actually have to forward with no budget left
    // (surfaced as a handler error that becomes an `ERROR_ACK`).
    let local_mr = match shared.local_mr {
        Some(m) => m,
        None => {
            let _ = send_error_ack(
                &shared,
                src_addr,
                header.request_id,
                "local backing not registered",
            );
            return;
        }
    };

    let src = header.source();
    let mut stream = handler.handle(&req, src, header.ttl);
    // SAFETY: stream is owned by this stack frame and pinned for
    // the duration of run_worker; we never move it.
    let mut stream = unsafe { std::pin::Pin::new_unchecked(&mut stream) };

    let mut next_idx: u32 = 0;
    loop {
        if shared.shutdown.load(Ordering::Acquire) {
            let _ = send_error_ack(&shared, src_addr, header.request_id, "server shutting down");
            return;
        }
        let waker = noop_waker();
        let mut task_cx = std::task::Context::from_waker(&waker);
        match stream.as_mut().poll_next(&mut task_cx) {
            std::task::Poll::Ready(Some(Ok(page))) => {
                match write_page(&shared, &local_mr, src_addr, &header, next_idx, page) {
                    Ok(()) => {
                        next_idx = next_idx.saturating_add(1);
                    }
                    Err(e) => {
                        let _ = send_error_ack(
                            &shared,
                            src_addr,
                            header.request_id,
                            &format!("write_page: {e}"),
                        );
                        return;
                    }
                }
            }
            std::task::Poll::Ready(Some(Err(e))) => {
                let _ = send_error_ack(&shared, src_addr, header.request_id, &format!("{e}"));
                return;
            }
            std::task::Poll::Ready(None) => {
                let _ = send_response_end(&shared, src_addr, header.request_id);
                return;
            }
            std::task::Poll::Pending => {
                std::thread::sleep(Duration::from_millis(5));
            }
        }
    }
}

fn write_page(
    shared: &Arc<ServerShared>,
    local_mr: &MrHandle,
    dest_fi_addr: u64,
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

    let inner = &shared.fabric;
    // SAFETY: local_mr.mr is owned by the live fabric.
    let desc = unsafe { ffi::ub_fi_mr_desc(local_mr.mr) };
    // The reverse-direction `fi_write` can fail transiently while the
    // `tcp` RDM connection back to the requester is still being
    // established: the submit can return `-FI_EAGAIN`, and an accepted
    // write can complete with a CQ error carrying `prov_errno=ENOTCONN`.
    // Both clear once the progress thread finishes the handshake, so we
    // retry the whole op until it succeeds or a deadline elapses.
    let deadline = std::time::Instant::now() + Duration::from_secs(10);
    loop {
        let (slot, fut) = inner.completions.allocate()?;
        let ctx = slot.into_raw();
        let rc = unsafe {
            ffi::ub_fi_write(
                inner.ep(),
                src_ptr,
                plan.len,
                desc,
                dest_fi_addr,
                plan.dest_addr,
                header.dest_mr_key,
                ctx,
            )
        };
        if rc < 0 {
            // SAFETY: just produced.
            let _ = unsafe { CompletionSlot::from_raw(ctx) };
            if rc as i32 == -ffi::FI_EAGAIN && std::time::Instant::now() < deadline {
                std::thread::sleep(Duration::from_millis(1));
                continue;
            }
            return Err(FabricError::Pkg("fi_write", rc as i32));
        }
        match block_on(fut, Duration::from_secs(10)) {
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

    let payload = bincode::serialize(&PageAck {
        page_idx: plan.ack_page_idx,
    })
    .map_err(|_| FabricError::Pkg("bincode(PageAck)", 0))?;
    submit_small_send(
        shared,
        dest_fi_addr,
        PAGE_ACK_TAG_BASE | (header.request_id as u64),
        &payload,
    )
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

fn send_response_end(shared: &Arc<ServerShared>, dest: u64, request_id: u32) -> FabResult<()> {
    submit_small_send(
        shared,
        dest,
        RESPONSE_END_TAG_BASE | (request_id as u64),
        &[],
    )
}

fn send_error_ack(
    shared: &Arc<ServerShared>,
    dest: u64,
    request_id: u32,
    msg: &str,
) -> FabResult<()> {
    let payload = bincode::serialize(&ErrorAck {
        message: msg.to_string(),
    })
    .map_err(|_| FabricError::Pkg("bincode(ErrorAck)", 0))?;
    submit_small_send(
        shared,
        dest,
        ERROR_ACK_TAG_BASE | (request_id as u64),
        &payload,
    )
}

fn submit_small_send(
    shared: &Arc<ServerShared>,
    dest: u64,
    tag: u64,
    payload: &[u8],
) -> FabResult<()> {
    let inner = &shared.fabric;
    let buf: Box<[u8]> = payload.to_vec().into_boxed_slice();
    let buf_len = buf.len();
    let buf_ptr = Box::into_raw(buf);
    let (slot, fut) = inner.completions.allocate()?;
    let buf_addr = buf_ptr as *mut u8 as usize;
    let buf_len_for_drop = buf_len;
    slot.set_handler(move |_| {
        // SAFETY: just produced from Box::into_raw on Box<[u8]>.
        let _ = unsafe {
            Box::from_raw(
                std::slice::from_raw_parts_mut(buf_addr as *mut u8, buf_len_for_drop)
                    as *mut [u8],
            )
        };
    });
    let ctx = slot.into_raw();
    // Retry on `-FI_EAGAIN` while the `tcp` RDM connection back to the
    // requester finishes establishing or the transmit queue drains.
    // EAGAIN neither consumes the buffer nor produces a completion, so
    // the same ctx/buffer are reused across attempts.
    let rc = {
        let deadline = std::time::Instant::now() + Duration::from_secs(10);
        loop {
            let rc = unsafe {
                ffi::ub_fi_tsend(
                    inner.ep(),
                    buf_ptr as *const std::ffi::c_void,
                    buf_len,
                    ptr::null_mut(),
                    dest,
                    tag,
                    ctx,
                )
            };
            if rc as i32 != -ffi::FI_EAGAIN || std::time::Instant::now() >= deadline {
                break rc;
            }
            std::thread::sleep(Duration::from_millis(1));
        }
    };
    if rc < 0 {
        // SAFETY: just produced.
        let _ = unsafe { CompletionSlot::from_raw(ctx) };
        let _ = unsafe {
            Box::from_raw(std::slice::from_raw_parts_mut(buf_ptr as *mut u8, buf_len) as *mut [u8])
        };
        return Err(FabricError::Pkg("fi_tsend (small)", rc as i32));
    }
    wait_for_small_send(fut, Duration::from_secs(10))
}

fn wait_for_small_send(fut: CompletionFuture, timeout: Duration) -> FabResult<()> {
    block_on(fut, timeout).map(|_| ())
}

fn noop_waker() -> std::task::Waker {
    use std::task::{RawWaker, RawWakerVTable};
    fn no(_: *const ()) {}
    fn clone(_: *const ()) -> RawWaker {
        RawWaker::new(ptr::null(), &VT)
    }
    static VT: RawWakerVTable = RawWakerVTable::new(clone, no, no, no);
    // SAFETY: vtable never dereferences the data pointer.
    unsafe { std::task::Waker::from_raw(RawWaker::new(ptr::null(), &VT)) }
}

fn block_on(mut fut: CompletionFuture, timeout: Duration) -> FabResult<CompletionInfo> {
    use std::future::Future;
    use std::pin::Pin;
    use std::task::{Context, Poll};
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut fut = unsafe { Pin::new_unchecked(&mut fut) };
    let started = std::time::Instant::now();
    loop {
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                if started.elapsed() >= timeout {
                    return Err(FabricError::Timeout);
                }
                std::thread::sleep(Duration::from_millis(1));
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

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
            tag: PAGE_ACK_TAG_BASE,
            src_addr: 0,
            op_context: 0,
        }
    }

    #[test]
    fn wait_for_small_send_accepts_completion_success() {
        let fut = completion_future(Ok(completion_info()));

        assert!(wait_for_small_send(fut, Duration::from_secs(1)).is_ok());
    }

    #[test]
    fn wait_for_small_send_propagates_completion_error() {
        let fut = completion_future(Err(FabricError::Cq {
            prov_errno: -3,
            err: -5,
        }));

        let err = wait_for_small_send(fut, Duration::from_secs(1)).unwrap_err();

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

        let err = wait_for_small_send(fut, Duration::ZERO).unwrap_err();

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

        let header = RequestHeader::new(7, &dest_mr, 3, src, 13);

        assert_eq!(header.request_id, 7);
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

        let header = RequestHeader::new(9, &dest_mr, 5, src, 42);
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
    // contract `run_worker` relies on after the unconditional
    // `ttl == 0` short-circuit was removed: the *handler* decides the
    // hop limit. An owner-serve must yield a page even at `ttl == 0`;
    // a forward with no budget must surface an error (which
    // `run_worker` turns into an `ERROR_ACK`).

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
