// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Client-side `bufferpool::Transport<R>` implementation over the
//! fabric module. One transport per NUMA shard.
//!
//! Wire protocol (mirror of the server-side responder in `rpc.rs`),
//! carried over the connection-managed `FI_EP_MSG` transport. Every
//! message is an 8-byte [`MsgHeader`] (`kind` + `request_id`) followed
//! by a kind-specific body:
//!
//! 1. Client allocates a `request_id` (32 bits), registers an
//!    [`AckSink`] for it on the fabric's inbound dispatch, then frames
//!    a [`MsgKind::Request`] message whose body is a
//!    [`super::rpc::RequestHeader`] plus the bincode-encoded `R` and
//!    `fi_send`s it to the routed peer's connection.
//! 2. The server replies on the same connection with framed
//!    [`MsgKind::PageAck`] / [`MsgKind::ResponseEnd`] /
//!    [`MsgKind::ErrorAck`] messages. The per-connection `RecvPool`
//!    receives them and the inbound dispatch routes each to this
//!    stream's `AckSink` by `request_id`. The client posts no recvs of
//!    its own.
//! 3. For each `PageAck`, the stream yields `dsts[page_idx_from_body]`.
//!    Once every requested page is acked the stream resolves with
//!    `None` on the next poll, with no separate `RESPONSE_END` (the
//!    client knows `dsts.len()` and page acks arrive in order, so the
//!    final ack is the end of stream).
//! 4. `RESPONSE_END` (sent only for a short success that delivered
//!    fewer than `dsts.len()` pages, including zero) resolves the
//!    stream with `None`; `ERROR_ACK` yields `Some(Err(_))` and then
//!    `None`.
//!
//! Drop: the stream unregisters its `AckSink` from the inbound
//! dispatch so no late reply is routed to a dead stream. There are no
//! client-posted recv contexts to cancel.
//!
//! **MR strategy**: page data lands directly in the caller-provided
//! `MrHandle` (the buffer pool's registered backing) via server-side
//! `fi_write`; the transport never copies page bytes through its own
//! buffers. The per-operation request control buffer is a heap
//! `Box<[u8]>` freed by the send completion handler. The
//! provider-required local descriptor is satisfied with `desc=NULL`.

use std::collections::VecDeque;
use std::marker::PhantomData;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use serde::Serialize;
use serde::de::DeserializeOwned;

use crate::bufferpool::{BulkRef, Error as PoolError, PageRef, PageStream, Req, Transport};
use crate::fabric::PeerId;

use super::backing::MrHandle;
use super::dispatch::{AckSink, InboundDispatch};
use super::error::{FabricError, Result as FabResult};
use super::fabric::Fabric;
use super::ffi;
use super::rpc::{ErrorAck, PageAck, RequestHeader};
use super::wire::{MsgHeader, MsgKind};

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
    page_size: usize,
    _marker: PhantomData<fn() -> R>,
}

impl<R, P> FabricTransport<R, P> {
    /// Build a transport bound to `fabric` with destination MR `mr`.
    /// `page_size` must divide `mr.len`. It is the page geometry the
    /// pool and the server-side wire planner share: the server
    /// addresses each destination page as `dest_mr_base + ordinal *
    /// page_size`, and the originating client uses `page_size` to shift
    /// `dest_mr_base` to the caller's physical slot (see
    /// `bulk_get_with_ttl`).
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
            page_size,
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
        self.bulk_get_with_ttl(req, src, dsts, crate::fabric::MAX_HOPS)
    }
}

impl<R, P> FabricTransport<R, P>
where
    R: Req + Serialize + DeserializeOwned + 'static,
    P: PeerRouter<R>,
{
    /// Build a `bulk_get` stream that seeds the request header's TTL
    /// (hop budget) with `ttl`. The plain [`Transport::bulk_get`]
    /// entry point delegates here with [`crate::fabric::MAX_HOPS`];
    /// the recursive-forwarding handler uses this to forward with a
    /// decremented `hops_remaining`.
    ///
    /// The wire protocol addresses each destination page as
    /// `dest_mr_base + ordinal * page_size`, but the pool hands us
    /// `dsts` pointing at arbitrary physical slots from its free list.
    /// We therefore shift `dest_mr_base` by `dsts[0].page_idx *
    /// page_size` so the server's ordinal-relative writes land in the
    /// caller's actual slots, exactly as [`Self::bulk_get_forward`]
    /// does for a recursive relay. This requires the `dsts` slots to be
    /// physically contiguous (the pool's single-page fetches trivially
    /// satisfy this); see the debug assertion below.
    pub(crate) fn bulk_get_with_ttl<'a>(
        &'a self,
        req: &'a R,
        src: BulkRef,
        dsts: &'a [PageRef],
        ttl: u32,
    ) -> FabricBulkStream<'a, R> {
        let dest_mr = match dsts.first() {
            Some(first) => {
                debug_assert!(
                    dsts.iter()
                        .enumerate()
                        .all(|(i, d)| { d.page_idx as usize == first.page_idx as usize + i }),
                    "fabric bulk_get requires physically contiguous dst slots; \
                     the wire protocol addresses pages by ordinal from dest_mr_base",
                );
                forward_dest_mr(self.mr, first.page_idx as usize * self.page_size)
            }
            None => self.mr,
        };
        FabricBulkStream::new_unstarted(self, req, src, dsts, ttl, dest_mr)
    }

    /// Forward `req` to the next hop, landing the downstream page(s)
    /// at byte offset `dest_offset` within this transport's MR rather
    /// than at offset 0.
    ///
    /// The wire protocol addresses each destination page as
    /// `dest_mr_base + ordinal * page_size`, so a recursive relay that
    /// reserved scratch slot `idx` must shift `dest_mr_base` by
    /// `idx * page_size`. Otherwise the downstream hop would RDMA-write
    /// into scratch slot 0 while the relay reads back slot `idx`,
    /// relaying stale bytes upstream (and colliding across concurrent
    /// forwards). The base is shifted by overriding the destination MR
    /// handle; `remote_key` and the underlying `fid_mr` are unchanged
    /// because the whole scratch backing is one registered region.
    pub(crate) fn bulk_get_forward<'a>(
        &'a self,
        req: &'a R,
        src: BulkRef,
        dsts: &'a [PageRef],
        ttl: u32,
        dest_offset: usize,
    ) -> FabricBulkStream<'a, R> {
        let dest_mr = forward_dest_mr(self.mr, dest_offset);
        FabricBulkStream::new_unstarted(self, req, src, dsts, ttl, dest_mr)
    }
}

/// Shift a destination MR handle forward by `dest_offset` bytes so a
/// recursive relay can land downstream pages at a non-zero scratch
/// slot. `remote_key` and the underlying `fid_mr` are preserved because
/// the whole scratch backing is registered as a single region.
fn forward_dest_mr(mr: MrHandle, dest_offset: usize) -> MrHandle {
    MrHandle {
        mr: mr.mr,
        remote_key: mr.remote_key,
        base: mr.base.saturating_add(dest_offset),
        remote_base: mr.remote_base.saturating_add(dest_offset as u64),
        len: mr.len.saturating_sub(dest_offset),
    }
}

/// Shared state between the stream and the inbound dispatch, which
/// delivers acks from the progress thread. Implements [`AckSink`] so
/// the dispatch can route replies straight into the queue.
pub(crate) struct StreamShared {
    /// FIFO of reply messages arriving from the dispatch.
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

impl AckSink for StreamShared {
    fn deliver(&self, kind: MsgKind, body: &[u8]) {
        self.push(RecvCompletion {
            kind,
            payload: body.to_vec(),
        });
    }

    fn deliver_page(&self, ordinal: u32) {
        // A page-landed write-with-immediate carries no framed body.
        // Re-encode it as the internal `PAGE_ACK` event so the poll
        // loop's existing ack bookkeeping (bounds, dedup, end-of-stream)
        // handles it verbatim. A serialize failure is impossible for a
        // fixed-size struct; fall back to an empty body, which the poll
        // loop rejects as a malformed ack rather than panicking on the
        // progress thread.
        let payload = bincode::serialize(&PageAck { page_idx: ordinal }).unwrap_or_default();
        self.push(RecvCompletion {
            kind: MsgKind::PageAck,
            payload,
        });
    }
}

struct RecvCompletion {
    kind: MsgKind,
    payload: Vec<u8>,
}

/// Stream returned by `FabricTransport::bulk_get`. On first poll it
/// registers the ack sink and sends the request; subsequent polls
/// drain replies and yield pages.
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
    /// Pre-launch state: nothing sent yet. We hold &Transport so the
    /// first poll can drive submission.
    Pending {
        fabric: Arc<Fabric>,
        mr: MrHandle,
        peer: PeerId,
        request_id: u32,
        src: BulkRef,
        req_bytes: Vec<u8>,
        dsts: &'a [PageRef],
        ttl: u32,
    },
    /// Active state: ack sink registered, request sent, awaiting acks.
    Active {
        shared: Arc<StreamShared>,
        dsts: &'a [PageRef],
        /// Dispatch we registered the sink on; Drop unregisters here.
        dispatch: Arc<InboundDispatch>,
        request_id: u32,
        /// Recycled 16-bit page handle bound to this stream's sink for
        /// the write-with-immediate path; Drop releases it.
        page_handle: u16,
        acked: Vec<bool>,
        ended: bool,
        emitted_error: bool,
        log: Option<crate::obs::ReqLog>,
    },
    Done,
}

impl<'a, R> FabricBulkStream<'a, R>
where
    R: Req + Serialize,
{
    fn new_unstarted<P>(
        transport: &FabricTransport<R, P>,
        req: &'a R,
        src: BulkRef,
        dsts: &'a [PageRef],
        ttl: u32,
        mr: MrHandle,
    ) -> Self
    where
        P: PeerRouter<R>,
    {
        // Allocate request_id eagerly so it's stable for the stream's
        // lifetime. Error materialization waits until first poll so
        // the trait signature stays infallible. The id is drawn from the
        // fabric-wide dispatch allocator so it is unique across every
        // transport sharing this fabric (see `alloc_request_id`).
        let request_id = transport.fabric.dispatch().alloc_request_id();
        let (peer, req_bytes) = match route_and_serialize(&transport.router, req) {
            Ok(v) => v,
            Err(error) => return Self::failed(error),
        };

        Self {
            state: StreamState::Pending {
                fabric: transport.fabric.clone(),
                mr,
                peer,
                request_id,
                src,
                req_bytes,
                dsts,
                ttl,
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
                    let (fabric, mr, peer, request_id, src, req_bytes, dsts, ttl) = match prev {
                        StreamState::Pending {
                            fabric,
                            mr,
                            peer,
                            request_id,
                            src,
                            req_bytes,
                            dsts,
                            ttl,
                        } => (fabric, mr, peer, request_id, src, req_bytes, dsts, ttl),
                        _ => unreachable!(),
                    };
                    let mut log = crate::obs::ReqLog::new("fabric.fetch");
                    log.field("peer", peer.0)
                        .field("req_id", request_id)
                        .hexkey("stripe", &src.stripe.0)
                        .field("off", src.offset)
                        .field("len", src.len)
                        .field("pages", dsts.len())
                        .field("ttl", ttl);
                    match launch(&fabric, &mr, peer, request_id, src, &req_bytes, dsts, ttl) {
                        Ok((shared, page_handle)) => {
                            this.state = StreamState::Active {
                                shared,
                                dsts,
                                dispatch: Arc::clone(fabric.dispatch()),
                                request_id,
                                page_handle,
                                acked: vec![false; dsts.len()],
                                ended: false,
                                emitted_error: false,
                                log: Some(log),
                            };
                            // Fall through to poll the Active state.
                        }
                        Err(e) => {
                            log.finish_err(&e);
                            return Poll::Ready(Some(Err(PoolError::transport(e))));
                        }
                    }
                }
                StreamState::Active {
                    shared,
                    dsts,
                    dispatch: _,
                    request_id: _,
                    page_handle: _,
                    acked,
                    ended,
                    emitted_error,
                    log,
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
                        Some(RecvCompletion { kind, payload }) => match kind {
                            MsgKind::PageAck => {
                                let ack: PageAck = match bincode::deserialize(&payload) {
                                    Ok(v) => v,
                                    Err(_) => {
                                        *ended = true;
                                        if let Some(mut l) = log.take() {
                                            l.finish_err("malformed PAGE_ACK");
                                        }
                                        return Poll::Ready(Some(Err(PoolError::transport(
                                            FabricError::BadConfig("malformed PAGE_ACK"),
                                        ))));
                                    }
                                };
                                let idx = ack.page_idx as usize;
                                if idx >= dsts.len() {
                                    *ended = true;
                                    if let Some(mut l) = log.take() {
                                        l.finish_err("PAGE_ACK index out of range");
                                    }
                                    return Poll::Ready(Some(Err(PoolError::transport(
                                        FabricError::BadConfig("PAGE_ACK index out of range"),
                                    ))));
                                }
                                if acked[idx] {
                                    *ended = true;
                                    if let Some(mut l) = log.take() {
                                        l.finish_err("duplicate PAGE_ACK index");
                                    }
                                    return Poll::Ready(Some(Err(PoolError::transport(
                                        FabricError::BadConfig("duplicate PAGE_ACK index"),
                                    ))));
                                }
                                acked[idx] = true;
                                // Full success carries no RESPONSE_END;
                                // the last in-order ack ends the stream.
                                if acked.iter().all(|a| *a) {
                                    *ended = true;
                                    if let Some(mut l) = log.take() {
                                        l.field("acked", acked.len()).finish_ok();
                                    }
                                }
                                return Poll::Ready(Some(Ok(dsts[idx])));
                            }
                            MsgKind::ResponseEnd => {
                                *ended = true;
                                if let Some(mut l) = log.take() {
                                    l.field("acked", acked.iter().filter(|a| **a).count())
                                        .finish_ok();
                                }
                                return Poll::Ready(None);
                            }
                            MsgKind::ErrorAck => {
                                *ended = true;
                                if *emitted_error {
                                    return Poll::Ready(None);
                                }
                                *emitted_error = true;
                                let msg = bincode::deserialize::<ErrorAck>(&payload)
                                    .map(|e| e.message)
                                    .unwrap_or_else(|_| "server error (undecodable)".into());
                                if let Some(mut l) = log.take() {
                                    l.finish_err(&msg);
                                }
                                return Poll::Ready(Some(Err(error_ack_to_pool_error(msg))));
                            }
                            // A REQUEST is never routed to a client ack
                            // sink; ignore any stray delivery.
                            MsgKind::Request => continue,
                        },
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
        if let StreamState::Active {
            dispatch,
            request_id,
            page_handle,
            log,
            ..
        } = &mut self.state
        {
            if let Some(mut l) = log.take() {
                l.finish_err("stream dropped before completion");
            }
            // Drop the dispatch entries so no late reply is routed to a
            // dead stream: the framed terminator sink and the recycled
            // page handle both reference this stream's `StreamShared`.
            dispatch.unregister_stream(*request_id);
            dispatch.free_page_handle(*page_handle);
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

fn error_ack_to_pool_error(message: String) -> PoolError {
    if message.ends_with("origin object not found") {
        return PoolError::OriginNotFound;
    }
    PoolError::transport(ServerError(message))
}

/// Register the ack sink and submit the framed request. Returns the
/// shared state the stream drains. On any failure the dispatch entry is
/// rolled back so a failed launch leaks nothing.
fn launch(
    fabric: &Arc<Fabric>,
    mr: &MrHandle,
    peer: PeerId,
    request_id: u32,
    src: BulkRef,
    req_bytes: &[u8],
    dsts: &[PageRef],
    ttl: u32,
) -> FabResult<(Arc<StreamShared>, u16)> {
    ensure_launch_fits_registry(fabric.completions().available_count(), dsts.len())?;

    // The page ordinal occupies the low 16 bits of the 32-bit immediate,
    // so a request can address at most 2^16 destination pages.
    if dsts.len() > u16::MAX as usize + 1 {
        return Err(FabricError::BadConfig(
            "bulk_get request exceeds 16-bit page ordinal space",
        ));
    }

    let (ep, _addr) = fabric.resolve_peer(peer)?;

    let shared = Arc::new(StreamShared {
        queue: Mutex::new(VecDeque::new()),
        waker: Mutex::new(None),
    });

    // Register the ack sink BEFORE sending so no reply can race ahead
    // of the dispatch entry. The same sink is bound to both the
    // request_id (for framed RESPONSE_END/ERROR_ACK terminators) and a
    // recycled 16-bit page handle (for write-with-immediate page-landed
    // signals).
    let sink: Arc<dyn AckSink> = shared.clone();
    fabric.dispatch().register_stream(request_id, sink.clone());
    let page_handle = match fabric.dispatch().alloc_page_handle(sink) {
        Some(h) => h,
        None => {
            fabric.dispatch().unregister_stream(request_id);
            return Err(FabricError::BadConfig("page handle space exhausted"));
        }
    };

    // Build the request body: header followed by the bincode-serialized
    // req body, then frame it with the message header.
    let header = RequestHeader::new(request_id, page_handle, mr, dsts.len() as u32, src, ttl);
    let mut body: Vec<u8> = bincode::serialize(&header).map_err(|e| {
        fabric.dispatch().free_page_handle(page_handle);
        fabric.dispatch().unregister_stream(request_id);
        FabricError::Encode(Arc::new(std::io::Error::new(
            std::io::ErrorKind::Other,
            format!("header encode: {e}"),
        )))
    })?;
    body.extend_from_slice(req_bytes);
    let framed = MsgHeader::frame(MsgKind::Request, request_id, &body);

    match send_request(fabric, ep, framed) {
        Ok(()) => Ok((shared, page_handle)),
        Err(e) => {
            fabric.dispatch().free_page_handle(page_handle);
            fabric.dispatch().unregister_stream(request_id);
            Err(e)
        }
    }
}

/// `fi_send` the framed request buffer on `ep` via the fabric's
/// pre-registered send pool, which frees/recycles the buffer from the
/// completion handler and registers a transient local MR only on the
/// fallback path. The first send to a peer can return `-FI_EAGAIN`
/// while the transmit queue is briefly full; the pool retries the same
/// operation up to a bounded deadline.
fn send_request(fabric: &Arc<Fabric>, ep: *mut ffi::fid_ep, framed: Vec<u8>) -> FabResult<()> {
    fabric
        .inner()
        .send_pool()
        .send_framed(
            ep,
            framed,
            "fi_send (request)",
            std::time::Duration::from_secs(10),
        )
        .map(|_fut| ())
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

/// Completion slots the client consumes for one `bulk_get`. Under the
/// `FI_EP_MSG` protocol the client posts no recvs of its own (the
/// per-connection `RecvPool` receives all replies), so a request costs
/// exactly one slot: the request send. The `dst_count` is retained for
/// API stability but no longer affects the count.
#[doc(hidden)]
pub fn required_completion_slots(dst_count: usize) -> Option<usize> {
    let _ = dst_count;
    Some(1)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::noop_waker;
    use serde::ser::Serializer;

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
        assert_eq!(acked.len(), dsts.len());
        FabricBulkStream {
            state: StreamState::Active {
                shared: Arc::new(StreamShared {
                    queue: Mutex::new(VecDeque::new()),
                    waker: Mutex::new(None),
                }),
                dsts,
                dispatch: InboundDispatch::new(),
                request_id: 1,
                page_handle: 0,
                acked,
                ended: false,
                emitted_error: false,
                log: None,
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
            kind: MsgKind::PageAck,
            payload,
        });
    }

    fn push_response_end(stream: &mut FabricBulkStream<'_, ()>) {
        let StreamState::Active { shared, .. } = &stream.state else {
            panic!("expected active stream");
        };
        shared.push(RecvCompletion {
            kind: MsgKind::ResponseEnd,
            payload: Vec::new(),
        });
    }

    fn push_error_ack(stream: &mut FabricBulkStream<'_, ()>, message: &str) {
        let StreamState::Active { shared, .. } = &stream.state else {
            panic!("expected active stream");
        };
        let payload = bincode::serialize(&ErrorAck {
            message: message.to_string(),
        })
        .expect("serialize error ack");
        shared.push(RecvCompletion {
            kind: MsgKind::ErrorAck,
            payload,
        });
    }

    fn push_page_landed(stream: &mut FabricBulkStream<'_, ()>, ordinal: u32) {
        let StreamState::Active { shared, .. } = &stream.state else {
            panic!("expected active stream");
        };
        // Drive the write-with-immediate page-landed path the inbound
        // dispatch invokes for an RDMA immediate completion.
        AckSink::deliver_page(shared.as_ref(), ordinal);
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
    fn required_completion_slots_counts_request_send_only() {
        assert_eq!(required_completion_slots(0), Some(1));
        assert_eq!(required_completion_slots(1), Some(1));
        assert_eq!(required_completion_slots(128), Some(1));
    }

    #[test]
    fn launch_available_slot_preflight_accepts_boundary() {
        assert!(ensure_launch_fits_registry(1, 0).is_ok());
        assert!(ensure_launch_fits_registry(1, 128).is_ok());
    }

    #[test]
    fn launch_available_slot_preflight_rejects_empty_registry() {
        let err = ensure_launch_fits_registry(0, 0).unwrap_err();
        assert!(matches!(
            err,
            FabricError::BadConfig("bulk_get request exceeds available completion slots")
        ));
    }

    #[test]
    fn response_end_with_no_pages_returns_eof() {
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
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(None)
        ));
    }

    #[test]
    fn error_ack_origin_not_found_preserves_pool_error() {
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        }];
        let mut stream = active_stream(&dsts, vec![false]);
        push_error_ack(&mut stream, "origin backend: origin object not found");

        assert!(matches!(
            poll_once(&mut stream),
            Poll::Ready(Some(Err(PoolError::OriginNotFound)))
        ));
        assert!(matches!(poll_once(&mut stream), Poll::Ready(None)));
    }

    #[test]
    fn error_ack_forwarded_origin_not_found_preserves_pool_error() {
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        }];
        let mut stream = active_stream(&dsts, vec![false]);
        push_error_ack(&mut stream, "forward to next hop: origin object not found");

        assert!(matches!(
            poll_once(&mut stream),
            Poll::Ready(Some(Err(PoolError::OriginNotFound)))
        ));
        assert!(matches!(poll_once(&mut stream), Poll::Ready(None)));
    }

    #[test]
    fn error_ack_other_message_remains_transport_error() {
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4096,
        }];
        let mut stream = active_stream(&dsts, vec![false]);
        push_error_ack(&mut stream, "backend exploded");

        match poll_once(&mut stream) {
            Poll::Ready(Some(Err(PoolError::Transport(err)))) => {
                assert!(
                    err.to_string().contains("backend exploded"),
                    "unexpected error: {err}",
                );
            }
            other => panic!("expected transport error, got {other:?}"),
        }
        assert!(matches!(poll_once(&mut stream), Poll::Ready(None)));
    }

    #[test]
    fn duplicate_page_ack_returns_transport_error() {
        // Two destinations so the stream is still active after the first
        // ack; a single-page stream self-terminates on "all pages acked"
        // before a duplicate could arrive.
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
    fn response_end_after_short_success_returns_eof() {
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
        push_page_ack(&mut stream, 0);
        push_response_end(&mut stream);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        // SAFETY: stream is pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(Some(Ok(PageRef { page_idx: 0, .. })))
        ));
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(None)
        ));
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
    fn all_pages_acked_ends_stream_without_response_end() {
        // Full success: every page is yielded, then None on the next
        // poll, with no terminator message.
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
        push_page_ack(&mut stream, 0);
        push_page_ack(&mut stream, 1);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        // SAFETY: stream is pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(Some(Ok(PageRef { page_idx: 0, .. })))
        ));
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(Some(Ok(PageRef { page_idx: 1, .. })))
        ));
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(None)
        ));
    }

    #[test]
    fn page_landed_immediate_yields_page_and_ends_stream() {
        // The write-with-immediate path delivers page ordinals through
        // `deliver_page`; the stream must yield each destination page and
        // self-terminate once all pages have landed, with no terminator.
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
        push_page_landed(&mut stream, 0);
        push_page_landed(&mut stream, 1);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        // SAFETY: stream is pinned on the stack and never moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(Some(Ok(PageRef { page_idx: 0, .. })))
        ));
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(Some(Ok(PageRef { page_idx: 1, .. })))
        ));
        assert!(matches!(
            stream.as_mut().poll_next(&mut cx),
            Poll::Ready(None)
        ));
    }

    #[test]
    fn forward_dest_mr_shifts_base_by_offset() {
        let mr = MrHandle {
            mr: std::ptr::null_mut(),
            remote_key: 0xABCD,
            base: 0x1000,
            remote_base: 0x1000,
            len: 0x8000,
        };
        let page_size = 0x1000usize;

        // Slot 0 leaves the handle untouched.
        let zero = forward_dest_mr(mr, 0);
        assert_eq!(zero.base, 0x1000);
        assert_eq!(zero.len, 0x8000);
        assert_eq!(zero.remote_key, 0xABCD);
        assert_eq!(zero.remote_base, 0x1000);

        // Slot 7 shifts base by 7 pages and shrinks len to match,
        // while preserving remote_key and the underlying fid_mr.
        let dest_offset = 7 * page_size;
        let shifted = forward_dest_mr(mr, dest_offset);
        assert_eq!(shifted.base, 0x1000 + dest_offset);
        assert_eq!(shifted.remote_base, 0x1000 + dest_offset as u64);
        assert_eq!(shifted.len, 0x8000 - dest_offset);
        assert_eq!(shifted.remote_key, 0xABCD);
        assert_eq!(shifted.mr, mr.mr);
    }

    #[test]
    fn forward_dest_mr_saturates_oversized_offset() {
        let mr = MrHandle {
            mr: std::ptr::null_mut(),
            remote_key: 1,
            base: 0,
            remote_base: 0,
            len: 0x1000,
        };
        // An offset beyond the region clamps len to zero rather than
        // wrapping; launch's bounds checks then reject the request.
        let shifted = forward_dest_mr(mr, 0x4000);
        assert_eq!(shifted.len, 0);
    }
}
