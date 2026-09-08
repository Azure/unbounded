// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Inbound message routing for the `FI_EP_MSG` transport.
//!
//! Under the connection-managed MSG model there is no tagged demux and
//! no address vector: every connection pre-posts a pool of fixed-size
//! untagged receives, and each completion is parsed for its
//! [`MsgHeader`](super::wire::MsgHeader) and routed by message kind.
//! This module owns that routing table so the receive pool
//! ([`super::recvpool`]) does not have to know about either the client
//! transport or the server.
//!
//! Two sinks meet here:
//!
//! - The **client** registers an [`AckSink`] per in-flight request
//!   (keyed by `request_id`) before it sends. Inbound `PAGE_ACK`,
//!   `RESPONSE_END`, and `ERROR_ACK` messages are delivered to the
//!   matching sink.
//! - The **server**, once started, installs a single [`RequestSink`].
//!   Inbound `REQUEST` messages are handed to it together with a
//!   [`ReplyCtx`] naming the connection they arrived on, so the
//!   server's RMA writes and acks go back out the same endpoint.
//!
//! The dispatch table is created during fabric bring-up and lives for
//! the fabric's lifetime; the server sink is installed later (when
//! `start_rpc_server` runs) and may be absent when the first
//! connections come up. A `REQUEST` that arrives with no server
//! installed is dropped (and counted), which is the correct behavior
//! for a node that is a pure client.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

use super::ffi;
use super::types::PeerId;
use super::wire::MsgKind;

/// Send-able wrapper around an active endpoint pointer. The endpoint is
/// owned by its [`Connection`](super::cm::Connection); this is a
/// non-owning handle used to post sends/writes back on the connection
/// from worker and progress threads.
#[derive(Clone, Copy)]
pub(crate) struct EpPtr(pub(crate) *mut ffi::fid_ep);

// SAFETY: libfabric endpoint operations are internally synchronized for
// a thread-safe domain (the fabric requires FI_THREAD_SAFE); this handle
// only ever calls those operations and never frees the endpoint.
unsafe impl Send for EpPtr {}
unsafe impl Sync for EpPtr {}

/// Everything the server needs to reply on the connection a request
/// arrived on: the originating endpoint and the peer identity. The
/// reply sends and RMA writes mint their operation contexts from the
/// fabric's shared completion registry, reached through `ServerShared`.
#[derive(Clone)]
pub(crate) struct ReplyCtx {
    pub(crate) ep: EpPtr,
    pub(crate) peer: PeerId,
}

impl ReplyCtx {
    pub(crate) fn new(ep: EpPtr, peer: PeerId) -> Self {
        ReplyCtx { ep, peer }
    }
}

/// Client-side delivery target for one in-flight request's acks. The
/// transport stream implements this; the dispatch table holds it only
/// while the request is outstanding.
pub(crate) trait AckSink: Send + Sync {
    /// Deliver one inbound control message (`RESPONSE_END` or
    /// `ERROR_ACK`) for this request. `body` is the message payload
    /// after the 8-byte header.
    fn deliver(&self, kind: MsgKind, body: Vec<u8>);

    /// Deliver one page-landed signal carried by an RDMA
    /// write-with-immediate completion (no framed message): `ordinal`
    /// is the page's index into the request's destination slice. This
    /// is the fast-path replacement for a framed `PAGE_ACK`.
    fn deliver_page(&self, ordinal: u32);
}

/// Server-side submission target for inbound requests. The RPC server
/// implements this and installs it via
/// [`InboundDispatch::install_server`].
pub(crate) trait RequestSink: Send + Sync {
    /// Hand off one inbound `REQUEST`. `reply` names the connection it
    /// arrived on; `body` is the payload after the 8-byte header
    /// (a `RequestHeader` prefix followed by the encoded request).
    fn submit(&self, reply: ReplyCtx, request_id: u32, body: Vec<u8>);
}

/// Routing table shared by every connection's receive pool. Created
/// during fabric bring-up; outlives all connections.
pub(crate) struct InboundDispatch {
    streams: Mutex<HashMap<u32, Arc<dyn AckSink>>>,
    page_streams: Mutex<PageHandles>,
    server: Mutex<Option<Arc<dyn RequestSink>>>,
    next_request_id: AtomicU32,
    dropped_requests: AtomicU64,
    orphan_acks: AtomicU64,
}

/// Recycled 16-bit handle space for the write-with-immediate page
/// path. The verbs provider exposes only a 32-bit immediate
/// (`cq_data_size == 4`), so a page-landed signal packs a 16-bit
/// request handle in the high half and the page ordinal in the low
/// half. A monotonic `request_id` would not fit and could alias once
/// truncated, so the client allocates a recycled handle here per
/// in-flight request (the in-flight depth is bounded far below 2^16).
#[derive(Default)]
struct PageHandles {
    slots: Vec<Option<Arc<dyn AckSink>>>,
    free: Vec<u16>,
}

impl InboundDispatch {
    pub(crate) fn new() -> Arc<Self> {
        Arc::new(Self {
            streams: Mutex::new(HashMap::new()),
            page_streams: Mutex::new(PageHandles::default()),
            server: Mutex::new(None),
            next_request_id: AtomicU32::new(1),
            dropped_requests: AtomicU64::new(0),
            orphan_acks: AtomicU64::new(0),
        })
    }

    /// Allocate a fabric-unique `request_id` for an outbound request.
    ///
    /// The `request_id` is the *only* demultiplexing key on the wire
    /// (the 8-byte [`MsgHeader`](super::wire::MsgHeader) carries just
    /// `kind` + `request_id`), and every connection's receive pool
    /// routes acks through this one dispatch table. The id space must
    /// therefore be unique across *all* client transports sharing a
    /// fabric, not per-transport: two `FabricTransport`s (e.g. one per
    /// shard) on the same fabric would otherwise both mint id 1, 2, ...
    /// and collide in `streams`, silently displacing one another's ack
    /// sink and hanging the displaced request. Allocating here, at the
    /// keyspace owner, guarantees uniqueness. The counter wraps at
    /// `u32::MAX`; with in-flight depth bounded far below 2^32 a wrapped
    /// id cannot alias a live request.
    pub(crate) fn alloc_request_id(&self) -> u32 {
        self.next_request_id.fetch_add(1, Ordering::Relaxed)
    }

    /// Install the server submission sink. Called once when the RPC
    /// server starts. Replaces any previous sink.
    pub(crate) fn install_server(&self, sink: Arc<dyn RequestSink>) {
        *self.server.lock().unwrap() = Some(sink);
    }

    /// Remove the server submission sink. Called when the RPC server
    /// shuts down. This is required to break the ownership cycle
    /// `FabricInner -> InboundDispatch -> RequestSink -> ServerShared ->
    /// FabricInner`: without it the fabric could never be dropped.
    /// Idempotent.
    pub(crate) fn uninstall_server(&self) {
        *self.server.lock().unwrap() = None;
    }

    /// Register an ack sink for `request_id`. The client must call this
    /// before sending the request so a fast reply cannot race ahead of
    /// the registration.
    pub(crate) fn register_stream(&self, request_id: u32, sink: Arc<dyn AckSink>) {
        self.streams.lock().unwrap().insert(request_id, sink);
    }

    /// Remove the ack sink for `request_id`. Idempotent.
    pub(crate) fn unregister_stream(&self, request_id: u32) {
        self.streams.lock().unwrap().remove(&request_id);
    }

    /// Allocate a recycled 16-bit page handle bound to `sink` for the
    /// write-with-immediate page path. Returns `None` only if 2^16
    /// handles are simultaneously live (far beyond any real in-flight
    /// depth), in which case the caller falls back to framed acks.
    pub(crate) fn alloc_page_handle(&self, sink: Arc<dyn AckSink>) -> Option<u16> {
        let mut h = self.page_streams.lock().unwrap();
        if let Some(handle) = h.free.pop() {
            h.slots[handle as usize] = Some(sink);
            Some(handle)
        } else if h.slots.len() <= u16::MAX as usize {
            let handle = h.slots.len() as u16;
            h.slots.push(Some(sink));
            Some(handle)
        } else {
            None
        }
    }

    /// Release a page handle obtained from [`alloc_page_handle`].
    /// Idempotent.
    pub(crate) fn free_page_handle(&self, handle: u16) {
        let mut h = self.page_streams.lock().unwrap();
        if (handle as usize) < h.slots.len() && h.slots[handle as usize].is_some() {
            h.slots[handle as usize] = None;
            h.free.push(handle);
        }
    }

    /// Route one page-landed signal decoded from a write-with-immediate
    /// completion: `handle` selects the request's sink, `ordinal` is the
    /// page index into its destination slice. A signal for an unknown
    /// handle (request already completed and freed) is counted as an
    /// orphan, never panics.
    pub(crate) fn route_page_landed(&self, handle: u16, ordinal: u32) {
        let sink = {
            let h = self.page_streams.lock().unwrap();
            h.slots.get(handle as usize).and_then(|s| s.clone())
        };
        match sink {
            Some(sink) => sink.deliver_page(ordinal),
            None => {
                self.orphan_acks.fetch_add(1, Ordering::Relaxed);
            }
        }
    }

    /// Route one fully-received inbound message. `kind`/`request_id`
    /// come from the parsed header; `body` is the payload after the
    /// header. `reply` names the connection the message arrived on (used
    /// only for `REQUEST`).
    pub(crate) fn route(&self, kind: MsgKind, request_id: u32, reply: &ReplyCtx, body: Vec<u8>) {
        match kind {
            MsgKind::Request => {
                let sink = self.server.lock().unwrap().clone();
                match sink {
                    Some(sink) => sink.submit(reply.clone(), request_id, body),
                    None => {
                        self.dropped_requests.fetch_add(1, Ordering::Relaxed);
                    }
                }
            }
            MsgKind::PageAck | MsgKind::ResponseEnd | MsgKind::ErrorAck => {
                let sink = self.streams.lock().unwrap().get(&request_id).cloned();
                match sink {
                    Some(sink) => sink.deliver(kind, body),
                    None => {
                        // A late ack for a request whose stream has
                        // already completed and unregistered. Benign.
                        self.orphan_acks.fetch_add(1, Ordering::Relaxed);
                    }
                }
            }
        }
    }

    #[cfg(test)]
    pub(crate) fn dropped_requests(&self) -> u64 {
        self.dropped_requests.load(Ordering::Relaxed)
    }

    #[cfg(test)]
    pub(crate) fn orphan_acks(&self) -> u64 {
        self.orphan_acks.load(Ordering::Relaxed)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::ptr;
    use std::sync::Mutex as StdMutex;

    fn reply_ctx() -> ReplyCtx {
        ReplyCtx {
            ep: EpPtr(ptr::null_mut()),
            peer: PeerId(1),
        }
    }

    #[derive(Default)]
    struct RecordingAck {
        got: StdMutex<Vec<(MsgKind, Vec<u8>)>>,
        pages: StdMutex<Vec<u32>>,
    }
    impl AckSink for RecordingAck {
        fn deliver(&self, kind: MsgKind, body: Vec<u8>) {
            self.got.lock().unwrap().push((kind, body));
        }
        fn deliver_page(&self, ordinal: u32) {
            self.pages.lock().unwrap().push(ordinal);
        }
    }

    #[derive(Default)]
    struct RecordingReq {
        got: StdMutex<Vec<(u32, Vec<u8>)>>,
    }
    impl RequestSink for RecordingReq {
        fn submit(&self, _reply: ReplyCtx, request_id: u32, body: Vec<u8>) {
            self.got.lock().unwrap().push((request_id, body));
        }
    }

    #[test]
    fn acks_route_to_the_registered_stream() {
        let d = InboundDispatch::new();
        let sink = Arc::new(RecordingAck::default());
        d.register_stream(7, sink.clone());

        d.route(MsgKind::PageAck, 7, &reply_ctx(), vec![1, 2, 3]);
        d.route(MsgKind::ResponseEnd, 7, &reply_ctx(), vec![]);

        let got = sink.got.lock().unwrap();
        assert_eq!(got.len(), 2);
        assert_eq!(got[0].0, MsgKind::PageAck);
        assert_eq!(got[0].1, vec![1, 2, 3]);
        assert_eq!(got[1].0, MsgKind::ResponseEnd);
    }

    #[test]
    fn acks_without_a_stream_are_counted_not_panicked() {
        let d = InboundDispatch::new();
        d.route(MsgKind::ErrorAck, 99, &reply_ctx(), vec![]);
        assert_eq!(d.orphan_acks(), 1);
    }

    #[test]
    fn unregister_stops_delivery() {
        let d = InboundDispatch::new();
        let sink = Arc::new(RecordingAck::default());
        d.register_stream(3, sink.clone());
        d.unregister_stream(3);
        d.route(MsgKind::PageAck, 3, &reply_ctx(), vec![9]);
        assert!(sink.got.lock().unwrap().is_empty());
        assert_eq!(d.orphan_acks(), 1);
    }

    #[test]
    fn requests_route_to_the_server_when_installed() {
        let d = InboundDispatch::new();
        let sink = Arc::new(RecordingReq::default());
        d.install_server(sink.clone());
        d.route(MsgKind::Request, 5, &reply_ctx(), vec![4, 5, 6]);
        let got = sink.got.lock().unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0], (5, vec![4, 5, 6]));
    }

    #[test]
    fn requests_without_a_server_are_dropped_and_counted() {
        let d = InboundDispatch::new();
        d.route(MsgKind::Request, 5, &reply_ctx(), vec![4, 5, 6]);
        assert_eq!(d.dropped_requests(), 1);
    }

    #[test]
    fn page_landed_routes_to_the_handle_owner() {
        let d = InboundDispatch::new();
        let sink = Arc::new(RecordingAck::default());
        let handle = d.alloc_page_handle(sink.clone()).unwrap();

        d.route_page_landed(handle, 0);
        d.route_page_landed(handle, 3);

        assert_eq!(*sink.pages.lock().unwrap(), vec![0, 3]);
    }

    #[test]
    fn page_handles_are_recycled() {
        let d = InboundDispatch::new();
        let a = Arc::new(RecordingAck::default());
        let h0 = d.alloc_page_handle(a.clone()).unwrap();
        d.free_page_handle(h0);
        let b = Arc::new(RecordingAck::default());
        let h1 = d.alloc_page_handle(b.clone()).unwrap();
        // The freed handle is reused rather than growing the table.
        assert_eq!(h0, h1);
    }

    #[test]
    fn page_landed_for_freed_handle_is_counted_not_panicked() {
        let d = InboundDispatch::new();
        let sink = Arc::new(RecordingAck::default());
        let handle = d.alloc_page_handle(sink.clone()).unwrap();
        d.free_page_handle(handle);
        d.route_page_landed(handle, 1);
        assert!(sink.pages.lock().unwrap().is_empty());
        assert_eq!(d.orphan_acks(), 1);
    }
}
