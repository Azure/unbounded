// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Client side of the bulk-get RPC: implements `bufferpool::Transport`
//! over Mercury.
//!
//! `MercuryTransport` is bound at construction time to a single
//! `ProgressContext` of a `Nic`, the page geometry of the local
//! backing, and a `PeerRouter` that maps an outbound request to a
//! known `PeerId`. Each `bulk_get` call resolves the peer address,
//! acquires a back-pressured completion slot from the context's
//! registry, builds a `BulkGetIn` describing the source range plus the
//! local destination page, issues `HG_Forward`, and then awaits the
//! forward callback. The server populates the destination via
//! `HG_Bulk_transfer(PUSH)` and only then responds; once the
//! `CompletionFuture` resolves successfully the bytes are already in
//! the destination page and the `BulkGetOut` status confirms it.
//!
//! The inline tests at the bottom of this file are intentionally
//! narrow: they cover argument validation that does not need a peer.
//! End-to-end loopback exercises live in `src/mercury/tests.rs`
//! (Wave 7), which spins up a listening Nic with the server side
//! attached and forwards to itself.

use std::marker::PhantomData;
use std::os::raw::c_void;
use std::sync::Arc;

use serde::Serialize;

use crate::bufferpool::{BulkRef, PageRef, Req, Transport};

use super::context::ProgressContext;
use super::error::HgError;
use super::ffi::{self, hg_addr_t, hg_cb_info, hg_return_t};
use super::handle::Handle;
use super::nic::Nic;
use super::progress::{CompletionSlot, from_callback_arg};
use super::router::PeerRouter;
use super::rpc::{BulkGetIn, BulkGetOut};

/// Client-side bulk-get transport.
///
/// Bound at construction to a single progress context, the backing's
/// page size, and a peer router. Cheap to clone (it is just a small
/// bundle of `Arc`s and a `u32`).
pub struct MercuryTransport<R, P>
where
    R: Req + Serialize,
    P: PeerRouter<R> + 'static,
{
    nic: Arc<Nic>,
    ctx: Arc<ProgressContext>,
    router: Arc<P>,
    page_size: u32,
    _phantom: PhantomData<R>,
}

impl<R, P> MercuryTransport<R, P>
where
    R: Req + Serialize,
    P: PeerRouter<R> + 'static,
{
    /// Bind a transport to a specific progress context within `nic`.
    ///
    /// `ctx_idx` selects an entry from `nic.contexts()`; constructing
    /// with an out-of-range index returns `HgError::BadConfig` rather
    /// than panicking. `page_size` must match the backing the embedder
    /// registered with the Nic via `Nic::register_backing`; the
    /// transport uses it to translate `(page_idx, intra_page_offset)`
    /// into a flat offset into the local bulk handle.
    pub fn new(
        nic: Arc<Nic>,
        ctx_idx: u16,
        page_size: u32,
        router: Arc<P>,
    ) -> Result<Self, HgError> {
        if page_size == 0 {
            return Err(HgError::BadConfig("MercuryTransport::new: page_size == 0"));
        }
        let contexts = nic.contexts();
        let idx = ctx_idx as usize;
        if idx >= contexts.len() {
            return Err(HgError::BadConfig(
                "MercuryTransport::new: ctx_idx out of range",
            ));
        }
        let ctx = Arc::clone(&contexts[idx]);
        Ok(Self {
            nic,
            ctx,
            router,
            page_size,
            _phantom: PhantomData,
        })
    }

    /// Bound progress context. Exposed primarily for tests that need
    /// to inspect the registry.
    pub fn context(&self) -> &Arc<ProgressContext> {
        &self.ctx
    }

    /// Bound page size, in bytes.
    pub fn page_size(&self) -> u32 {
        self.page_size
    }
}

impl<R, P> Transport<R> for MercuryTransport<R, P>
where
    R: Req + Serialize,
    P: PeerRouter<R> + 'static,
{
    async fn bulk_get(
        &self,
        req: &R,
        src: BulkRef,
        dst: PageRef,
    ) -> Result<(), crate::bufferpool::Error> {
        // Validate the destination page geometry up front. `BulkRef.len`
        // is `u32`; the destination must be wide enough to receive it
        // and must not extend past a single page.
        if dst.len < src.len {
            return Err(HgError::BadConfig("bulk_get: dst.len < src.len").into());
        }
        let page_size = self.page_size;
        let dst_end = dst
            .offset
            .checked_add(dst.len)
            .ok_or_else(|| HgError::BadConfig("bulk_get: dst offset+len overflow"))?;
        if dst_end > page_size {
            return Err(HgError::BadConfig("bulk_get: dst extends past page_size").into());
        }

        let local_bulk = self
            .nic
            .local_bulk()
            .ok_or_else(|| HgError::BadConfig("bulk_get: no backing registered"))?;

        let peer_id = self
            .router
            .route(req)
            .ok_or_else(|| HgError::BadConfig("bulk_get: router returned no peer"))?;
        let addr_nn = self
            .nic
            .peers()
            .get(peer_id)
            .ok_or(HgError::HgAddrLookup(0))?;
        // `PeerTable` stores the `hg_addr_t` value inside a
        // `NonNull<hg_addr_t>`; recover the raw addr by reinterpreting
        // the pointer's bit pattern. See `nic.rs::lookup_and_insert`
        // for the inverse construction.
        let addr: hg_addr_t = addr_nn.cast::<super::ffi::hg_addr_s>().as_ptr();

        let req_bytes = bincode::serialize(req).map_err(|_| HgError::Encode("bincode"))?;

        // Acquire a registry slot. The future drops the registry's
        // capacity counter exactly once on resolution or drop; the
        // FFI-bound clone of the `Arc<CompletionSlot>` is reclaimed by
        // `forward_cb` (or by us on submit failure below).
        let registry = Arc::clone(self.ctx.registry());
        let registered = registry.acquire().await;
        let raw_arg = registered.into_callback_arg();
        let fut = registered.into_future();

        // SAFETY of the unsafe blocks below:
        // * `input` lives on this stack frame and remains in place
        //   until `HG_Forward` returns (Mercury invokes the in-proc
        //   callback synchronously inside `HG_Forward` to encode the
        //   payload, after which the input buffer can move freely).
        // * `req_bytes` outlives the same window, so the raw pointer
        //   stored in `input.req_bytes` is valid for the encode pass.
        // * `addr` was looked up against `nic`'s class and is alive
        //   for as long as the `Arc<Nic>` we hold; the handle borrows
        //   it for the duration of the call.
        // * `raw_arg` is a strong `Arc<CompletionSlot>` clone, which
        //   `forward_cb` reclaims with `from_callback_arg`. On a
        //   submit failure we reclaim it ourselves below.
        let mut input = unsafe { BulkGetIn::zeroed() };
        input.stripe_key = src.stripe.0;
        input.stripe_off = src.offset;
        input.dst_offset = (dst.page_idx as u64)
            .checked_mul(page_size as u64)
            .and_then(|base| base.checked_add(dst.offset as u64))
            .ok_or_else(|| HgError::BadConfig("bulk_get: dst_offset overflow"))?;
        input.len = src.len;
        input.req_bytes_len = req_bytes.len() as u32;
        input.req_bytes = req_bytes.as_ptr() as *mut u8;
        input.dst_bulk = local_bulk.as_raw();

        let handle = Handle::create(self.ctx.raw(), addr, self.nic.rpc_id())?;

        // SAFETY: the input pointer is valid for the synchronous
        // duration of `HG_Forward` (Mercury encodes the payload before
        // returning); `raw_arg` is the strong arc the callback will
        // reclaim. See the SAFETY comment block above.
        let submitted = unsafe {
            handle.forward(
                Some(forward_cb),
                raw_arg,
                &mut input as *mut BulkGetIn as *mut c_void,
            )
        };
        if let Err(e) = submitted {
            // Forward never fired the callback, so the strong arc we
            // leaked into FFI never came home. Reclaim it here. The
            // `CompletionFuture` we built from `registered` still
            // releases the registry's capacity counter when dropped.
            // SAFETY: `raw_arg` was just produced by
            // `into_callback_arg`, has not been observed by any
            // callback, and is reclaimed exactly once here.
            unsafe {
                let _ = Arc::from_raw(raw_arg as *const CompletionSlot);
            }
            drop(fut);
            return Err(e.into());
        }

        // The encode pass has already copied `req_bytes` into Mercury's
        // wire buffer, so it is safe to drop after `HG_Forward`
        // returns. We do not drop it explicitly; lexical scope ends at
        // function exit.

        fut.await?;

        let mut out = BulkGetOut { status: 0 };
        // SAFETY: `out` is a properly-aligned `BulkGetOut` on this
        // stack frame; `handle` is still alive until end of scope.
        unsafe {
            handle.get_output(&mut out as *mut BulkGetOut as *mut c_void)?;
        }
        let status = out.status;
        // SAFETY: matched against the prior `get_output` on the same
        // handle; required to release Mercury-side allocations even
        // when `status != 0`.
        unsafe {
            handle.free_output(&mut out as *mut BulkGetOut as *mut c_void)?;
        }
        if status != 0 {
            return Err(HgError::HgRespond(status).into());
        }
        Ok(())
    }
}

/// `HG_Forward` completion callback.
///
/// Reclaims the `Arc<CompletionSlot>` strong reference handed to FFI
/// in `bulk_get`, publishes the outcome (success or `HgError::HgForward`),
/// and returns. Mercury releases its handle reference after the
/// callback returns.
unsafe extern "C" fn forward_cb(info: *const hg_cb_info) -> hg_return_t {
    ffi::dispatch_forward_cb(info, |cb| {
        if cb.arg.is_null() {
            return ffi::HG_SUCCESS;
        }
        // SAFETY: `cb.arg` is the pointer produced by
        // `RegisteredSlot::into_callback_arg` in `bulk_get`; it has
        // not been reclaimed elsewhere because the only other site
        // (the submit-failure path) is mutually exclusive with the
        // callback firing.
        let slot = unsafe { from_callback_arg(cb.arg) };
        let outcome = if cb.ret == ffi::HG_SUCCESS {
            Ok(())
        } else {
            Err(HgError::HgForward(cb.ret))
        };
        slot.complete(outcome);
        ffi::HG_SUCCESS
    })
}

// =====================================================================
// Tests
// =====================================================================

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::StripeKey;
    use crate::mercury::config::NicConfig;
    use crate::mercury::peer::PeerId;
    use crate::mercury::router::StaticPeer;
    use crate::runtime::{DefaultRuntime, WorkerIdx};
    use serde::Serialize;
    use std::future::Future;
    use std::pin::Pin;
    use std::task::{Context, Poll, Waker};

    // -- Noop waker / block_on, mirroring the patterns in progress.rs.

    fn noop_raw_waker() -> std::task::RawWaker {
        fn no_op(_: *const ()) {}
        fn clone(_: *const ()) -> std::task::RawWaker {
            noop_raw_waker()
        }
        let vt = &std::task::RawWakerVTable::new(clone, no_op, no_op, no_op);
        std::task::RawWaker::new(std::ptr::null(), vt)
    }

    fn noop_waker() -> Waker {
        // SAFETY: vtable is `'static`, all functions are no-ops, the
        // data pointer is never dereferenced.
        unsafe { Waker::from_raw(noop_raw_waker()) }
    }

    fn block_on<F: Future>(mut f: F) -> F::Output {
        // SAFETY: `f` is owned by this stack frame for the duration of
        // the call and is not moved after pinning.
        let mut f = unsafe { Pin::new_unchecked(&mut f) };
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        for _ in 0..1_000_000 {
            match f.as_mut().poll(&mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => continue,
            }
        }
        panic!("block_on: future did not complete within 1M polls");
    }

    // -- A minimal `Req` with `Serialize` for the tests below.

    #[derive(Serialize)]
    struct TestReq {
        key: [u8; 32],
    }

    impl Req for TestReq {
        fn key(&self) -> StripeKey {
            StripeKey(self.key)
        }
    }

    fn cfg() -> NicConfig {
        NicConfig {
            na_info_string: "na+sm".to_string(),
            listen: false,
            contexts_per_nic: 2,
            ..NicConfig::default()
        }
    }

    fn build_nic() -> Option<Arc<Nic>> {
        let rt = DefaultRuntime::new(1);
        match Nic::new(&cfg(), &*rt, WorkerIdx(0)) {
            Ok(n) => Some(Arc::new(n)),
            Err(HgError::HgInit(_)) => None,
            Err(e) => panic!("unexpected Nic::new error: {e:?}"),
        }
    }

    #[test]
    fn new_rejects_invalid_ctx_idx() {
        let nic = match build_nic() {
            Some(n) => n,
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };
        let router = Arc::new(StaticPeer(PeerId(1)));
        let err = MercuryTransport::<TestReq, _>::new(Arc::clone(&nic), 99, 4096, router)
            .err()
            .expect("ctx_idx 99 must be rejected");
        match err {
            HgError::BadConfig(_) => {}
            other => panic!("expected BadConfig, got {other:?}"),
        }
        nic.shutdown().expect("shutdown ok");
    }

    #[test]
    fn new_rejects_zero_page_size() {
        let nic = match build_nic() {
            Some(n) => n,
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };
        let router = Arc::new(StaticPeer(PeerId(1)));
        let err = MercuryTransport::<TestReq, _>::new(Arc::clone(&nic), 0, 0, router)
            .err()
            .expect("page_size 0 must be rejected");
        assert!(matches!(err, HgError::BadConfig(_)));
        nic.shutdown().expect("shutdown ok");
    }

    #[test]
    fn bulk_get_without_backing_returns_bad_config() {
        let nic = match build_nic() {
            Some(n) => n,
            None => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        };
        let router = Arc::new(StaticPeer(PeerId(1)));
        let transport = MercuryTransport::<TestReq, _>::new(Arc::clone(&nic), 0, 4096, router)
            .expect("transport built");

        let req = TestReq { key: [0u8; 32] };
        let src = BulkRef {
            stripe: StripeKey([0u8; 32]),
            offset: 0,
            len: 16,
        };
        let dst = PageRef {
            page_idx: 0,
            offset: 0,
            len: 16,
        };
        let res = block_on(transport.bulk_get(&req, src, dst));
        let err = res.err().expect("missing backing => Err");
        match err {
            crate::bufferpool::Error::Transport(inner) => {
                let msg = format!("{inner}");
                assert!(
                    msg.contains("no backing registered"),
                    "unexpected transport error: {msg}"
                );
            }
            other => panic!("expected Transport(_), got {other:?}"),
        }
        nic.shutdown().expect("shutdown ok");
    }
}
