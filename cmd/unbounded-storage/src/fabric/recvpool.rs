// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Per-connection receive pool for the `FI_EP_MSG` transport.
//!
//! Each connected endpoint keeps a fixed number of untagged
//! `fi_recv` buffers posted at all times. When one completes, the
//! progress thread runs the slot handler installed here: it parses the
//! [`MsgHeader`] prefix, hands the framed body to [`InboundDispatch`]
//! for routing (server request or client ack), and then immediately
//! re-arms the same buffer on a fresh completion slot so the pool depth
//! stays constant. This mirrors
//! the self-reposting request-recv pattern the tagged RDM server used,
//! but every buffer now serves all four message kinds and is demuxed by
//! the header rather than by tag.
//!
//! Buffer ownership: each posted recv owns one stable heap buffer captured
//! by its completion handler. On success the handler reads the buffer and
//! moves it into the replacement recv. On error (including the cancellation
//! libfabric delivers when the endpoint is closed) the handler frees its
//! buffer and does not re-arm, so a closing endpoint drains its pool to zero.
//!
//! Local descriptors: providers that negotiate `FI_MR_LOCAL` (verbs)
//! require every recv buffer to carry a `desc` from a registered region.
//! Each receive lane registers one `LocalMr` for its buffer and hands its
//! `desc` to every `fi_recv` repost; the completion handler moves the
//! `LocalMr` alongside the stable buffer. Providers without `FI_MR_LOCAL`
//! (tcp) negotiate it
//! off, so `LocalMrCtx::register` returns `None` and the recv posts with
//! `desc = NULL`.

use std::ffi::c_void;
use std::ptr;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use super::backing::{LocalMr, LocalMrCtx};
use super::completion::{CompletionInfo, CompletionRegistry};
use super::dispatch::{EpPtr, InboundDispatch, ReplyCtx};
use super::error::Result;
use super::ffi;
use super::types::PeerId;
use super::wire::{MSG_HEADER_LEN, MsgHeader, RECV_BUF_LEN};

/// Arms self-reposting untagged receives for one connected endpoint.
pub(crate) struct RecvPool;

struct RecvPoolShared {
    ep: EpPtr,
    peer: PeerId,
    completions: Arc<CompletionRegistry>,
    dispatch: Arc<InboundDispatch>,
    local_ctx: LocalMrCtx,
    errors: AtomicU64,
}

struct RecvBuffer {
    local_mr: Option<LocalMr>,
    bytes: Box<[u8; RECV_BUF_LEN]>,
}

impl RecvBuffer {
    fn new(local_ctx: &LocalMrCtx) -> Result<Self> {
        let mut bytes = Box::new([0u8; RECV_BUF_LEN]);
        let local_mr = local_ctx.register(
            bytes.as_mut_ptr() as *mut c_void,
            RECV_BUF_LEN,
            ffi::FI_RECV,
        )?;
        Ok(Self { local_mr, bytes })
    }

    fn ptr(&mut self) -> *mut c_void {
        self.bytes.as_mut_ptr() as *mut c_void
    }

    fn desc(&self) -> *mut c_void {
        self.local_mr
            .as_ref()
            .map(LocalMr::desc)
            .unwrap_or(ptr::null_mut())
    }
}

impl RecvPool {
    /// Arm up to `depth` receives on `ep`. Each completion routes
    /// through `dispatch` and re-arms itself. `peer` tags routed
    /// requests with the originating identity; `completions` is the
    /// registry the recv slots (and their re-arms) draw from.
    ///
    /// The provider's receive queue may refuse further posts with
    /// `EAGAIN` before `depth` is reached (the `tcp` provider draws
    /// posted recvs from a shared buffer pool that many concurrent
    /// endpoints contend for). That is not fatal: arming stops at the
    /// first `EAGAIN` and the pool runs at whatever depth the provider
    /// accepted, since every completion re-arms 1:1. As long as one
    /// recv is posted the connection can make progress. A genuine
    /// (non-`EAGAIN`) post failure, or `EAGAIN` on the very first post
    /// (nothing armed), is surfaced as an error.
    pub(crate) fn arm(
        ep: *mut ffi::fid_ep,
        peer: PeerId,
        completions: Arc<CompletionRegistry>,
        dispatch: Arc<InboundDispatch>,
        depth: usize,
        local_ctx: LocalMrCtx,
    ) -> Result<()> {
        let shared = Arc::new(RecvPoolShared {
            ep: EpPtr(ep),
            peer,
            completions,
            dispatch,
            local_ctx,
            errors: AtomicU64::new(0),
        });
        let mut posted = 0usize;
        for _ in 0..depth.max(1) {
            match shared.post_one() {
                Ok(true) => posted += 1,
                // Receive queue full: stop arming, run at current depth.
                Ok(false) => break,
                Err(e) => return Err(e),
            }
        }
        if posted == 0 {
            return Err(super::error::FabricError::Pkg("fi_recv", -unsafe {
                ffi::ub_fi_eagain()
            }));
        }
        Ok(())
    }
}

impl RecvPoolShared {
    /// Allocate one buffer + completion slot and post a single recv.
    /// The slot handler parses, routes, and re-arms.
    ///
    /// Returns `Ok(true)` when a recv was posted, `Ok(false)` when the
    /// provider's receive queue is momentarily full (`EAGAIN`; the slot
    /// and buffer are reclaimed and nothing is posted), and `Err` for a
    /// genuine post failure.
    fn post_one(self: &Arc<Self>) -> Result<bool> {
        // The future is intentionally dropped: this slot is driven by
        // its handler, not awaited. The registry slot stays live until
        // `complete` runs on the progress thread.
        let (slot, _fut) = self.completions.allocate()?;
        let recv = RecvBuffer::new(&self.local_ctx)?;
        self.post_buffer(slot, recv)
    }

    fn repost(self: &Arc<Self>, recv: RecvBuffer) -> Result<bool> {
        let (slot, _fut) = self.completions.allocate()?;
        self.post_buffer(slot, recv)
    }

    fn post_buffer(
        self: &Arc<Self>,
        mut slot: Box<super::completion::CompletionSlot>,
        mut recv: RecvBuffer,
    ) -> Result<bool> {
        let buf_ptr = recv.ptr();
        let desc = recv.desc();
        let shared = Arc::clone(self);
        slot.set_handler(move |result: &std::result::Result<CompletionInfo, _>| {
            match result {
                Ok(info) => {
                    // A write-with-immediate (page-landed signal) consumes
                    // this recv buffer but writes no frame into it; the
                    // page ordinal rides in the CQ immediate data. Route it
                    // by the packed handle instead of parsing a header.
                    let remote_write = if info.flags & ffi::FI_REMOTE_CQ_DATA != 0 {
                        Some(info.data)
                    } else {
                        None
                    };
                    let message = if remote_write.is_none() {
                        shared.decode_message(&recv.bytes[..], info.bytes)
                    } else {
                        None
                    };
                    // Re-arm the stable buffer and local MR on a fresh
                    // completion slot before dispatching. Dispatch may send an
                    // overload reply from this thread and must not consume the
                    // completion capacity reserved for the receive lane.
                    if !matches!(shared.repost(recv), Ok(true)) {
                        shared.errors.fetch_add(1, Ordering::Relaxed);
                    }
                    if let Some(data) = remote_write {
                        shared.handle_remote_write(data);
                    } else if let Some((header, body)) = message {
                        shared.handle_message(header, body);
                    }
                }
                Err(_) => {
                    // Cancellation on endpoint close, or a genuine recv
                    // error. Either way drop the buffer and stop; the
                    // pool drains as the endpoint tears down.
                    shared.errors.fetch_add(1, Ordering::Relaxed);
                }
            }
        });

        let ctx = slot.into_raw();
        let posted = unsafe {
            ffi::ub_fi_recv(
                self.ep.0,
                buf_ptr as *mut c_void,
                RECV_BUF_LEN,
                desc,
                ffi::FI_ADDR_UNSPEC,
                ctx,
            )
        };
        if posted < 0 {
            // Reclaim the leaked slot (and its captured buffer) so a
            // failed post does not leak.
            let slot = unsafe { super::completion::CompletionSlot::from_raw(ctx) };
            drop(slot);
            // EAGAIN is a soft "receive queue full" signal, not a hard
            // failure: the caller decides whether running at a shallower
            // depth is acceptable.
            if posted == -(unsafe { ffi::ub_fi_eagain() } as isize) {
                return Ok(false);
            }
            return Err(super::error::FabricError::Pkg("fi_recv", posted as i32));
        }
        Ok(true)
    }

    /// Parse the framed message in `buf[..n]` and route it. Malformed
    /// or truncated frames are dropped (counted as errors) rather than
    /// propagated, since there is no caller to surface them to on the
    /// progress thread.
    fn decode_message(&self, buf: &[u8], n: usize) -> Option<(MsgHeader, Vec<u8>)> {
        if n < MSG_HEADER_LEN {
            self.errors.fetch_add(1, Ordering::Relaxed);
            return None;
        }
        let header = match MsgHeader::read_from(&buf[..n]) {
            Ok(h) => h,
            Err(_) => {
                self.errors.fetch_add(1, Ordering::Relaxed);
                return None;
            }
        };
        let body = buf[MSG_HEADER_LEN..n].to_vec();
        Some((header, body))
    }

    fn handle_message(self: &Arc<Self>, header: MsgHeader, body: Vec<u8>) {
        let reply = ReplyCtx::new(self.ep, self.peer);
        self.dispatch
            .route(header.kind, header.request_id, &reply, body);
    }

    /// Route a page-landed signal carried by a write-with-immediate
    /// completion. The 32-bit immediate packs the request's page handle
    /// in the high 16 bits and the page ordinal in the low 16 bits
    /// (verbs reports `cq_data_size` = 4, so only 32 bits are available).
    fn handle_remote_write(self: &Arc<Self>, data: u64) {
        let handle = (data >> 16) as u16;
        let ordinal = (data & 0xFFFF) as u32;
        self.dispatch.route_page_landed(handle, ordinal);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn arm_on_null_ep_fails_without_leaking() {
        // A null endpoint makes fi_recv fail; arm must surface the error
        // and not leak the registry slot it reserved.
        let completions = CompletionRegistry::new(8);
        let dispatch = InboundDispatch::new();
        let before = completions.live_count();
        let res = RecvPool::arm(
            ptr::null_mut(),
            PeerId(1),
            Arc::clone(&completions),
            dispatch,
            4,
            LocalMrCtx::none(),
        );
        assert!(res.is_err());
        assert_eq!(completions.live_count(), before);
    }
}
