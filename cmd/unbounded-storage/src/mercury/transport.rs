// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! `bufferpool::Transport<R>` implementation over Mercury.
//!
//! Each `bulk_get` builds a `BulkGetIn`, creates a handle against the
//! routed peer, and forwards. The forward callback fires on the
//! Mercury progress thread and resolves the future via the
//! completion bridge. The client never deserializes the response
//! body; Mercury orders the bulk-push the server issued before the
//! `HG_Respond`, so a successful callback means the destination page
//! is fully populated.
//!
//! Construction binds the transport to a `Class` and a fixed
//! `page_size`. The embedder is expected to have already registered
//! the pool's backing with the class via `Class::register_backing`;
//! the transport just flattens `PageRef { page_idx, offset }` into a
//! byte offset using the page size baked in at `new()`.

use std::sync::Arc;

use serde::Serialize;

use crate::bufferpool::{BulkRef, Error as PoolError, PageRef, Req, Transport};
use crate::mercury::class::{Class, ClassInner};
use crate::mercury::error::{HgError, Result as HgResult};
use crate::mercury::ffi;
use crate::mercury::handle::Handle;
use crate::mercury::progress::{CompletionFuture, CompletionSlot};
use crate::mercury::router::PeerRouter;
use crate::mercury::rpc::{BulkGetIn, BulkGetOut};

/// Mercury-backed transport. One per NUMA shard. `R` is the
/// bufferpool request type; `P` selects the peer per request.
pub struct MercuryTransport<R, P>
where
    P: PeerRouter<R>,
    R: Req + Serialize,
{
    class: Arc<ClassInner>,
    router: P,
    /// Bound at construction; used to flatten `PageRef` into a byte
    /// offset for `bulk_get`. Must match the `page_size` the
    /// embedder passed when allocating the backing it registered
    /// with `class`.
    page_size: usize,
    _r: std::marker::PhantomData<fn() -> R>,
}

impl<R, P> MercuryTransport<R, P>
where
    P: PeerRouter<R>,
    R: Req + Serialize,
{
    /// Build a transport bound to `class` and `page_size`. The
    /// caller is responsible for having already registered the
    /// pool's backing with `class` (via `Class::register_backing`).
    /// `page_size` must equal the `Backing::page_size` of the pool
    /// that will own this transport.
    pub fn new(class: &Class, router: P, page_size: usize) -> Self {
        assert!(
            page_size > 0 && page_size.is_power_of_two(),
            "MercuryTransport::new: page_size must be a positive power of two",
        );
        Self {
            class: class.inner().clone(),
            router,
            page_size,
            _r: std::marker::PhantomData,
        }
    }
}

impl<R, P> Transport<R> for MercuryTransport<R, P>
where
    P: PeerRouter<R>,
    R: Req + Serialize,
{
    async fn bulk_get(
        &self,
        req: &R,
        src: BulkRef,
        dst: PageRef,
    ) -> std::result::Result<(), PoolError> {
        let peer = self.router.route(req).map_err(PoolError::transport)?;
        let page_size = self.page_size;
        let dst_offset = (dst.page_idx as u64)
            .checked_mul(page_size as u64)
            .and_then(|p| p.checked_add(dst.offset as u64))
            .ok_or(PoolError::OffsetOutOfRange)?;

        let req_bytes = bincode::serialize(req).map_err(|_| {
            // bufferpool::Error has no cause-chaining slot today; the
            // static `ctx` on `HgError` is enough to identify the
            // failure site. Extend `Error::transport` when we need
            // the inner cause.
            PoolError::transport(HgError::new(0, "bincode serialize failed"))
        })?;
        let local_bulk = self
            .class
            .local_bulk
            .lock()
            .expect("local_bulk mutex")
            .as_ref()
            .map(|b| b.handle())
            .ok_or_else(|| PoolError::transport(HgError::new(0, "backing not registered")))?;

        let addr = self.class.peer_addr(peer).map_err(PoolError::transport)?;

        let slot = self
            .class
            .completions
            .alloc()
            .map_err(PoolError::transport)?;
        let registry = self.class.completions.clone();

        forward_one(
            &self.class,
            addr,
            local_bulk,
            dst_offset,
            &src,
            &req_bytes,
            slot.clone(),
        )
        .map_err(PoolError::transport)?;

        CompletionFuture { slot, registry }
            .await
            .map_err(PoolError::transport)
    }
}

/// Internal helper: builds the input struct, creates the handle,
/// and submits the forward. On any error the slot is released so
/// the in-flight counter unblocks immediately.
fn forward_one(
    class: &Arc<ClassInner>,
    addr: ffi::hg_addr_t,
    local_bulk: ffi::hg_bulk_t,
    dst_offset: u64,
    src: &BulkRef,
    req_bytes: &[u8],
    slot: Arc<CompletionSlot>,
) -> HgResult<()> {
    let mut input = BulkGetIn {
        stripe_key: src.stripe.0,
        stripe_off: src.offset,
        dst_offset,
        len: src.len,
        req_bytes_len: req_bytes.len() as u32,
        req_bytes: req_bytes.as_ptr() as *mut u8,
        dst_bulk: local_bulk,
    };

    let handle = Handle::create(class, addr)?;
    // Hand the slot to C as a raw pointer. The forward callback reclaims
    // the strong reference and lets it drop.
    let arg = slot.into_callback_arg();
    if let Err(e) = handle.forward(forward_cb, arg, &mut input) {
        // The callback will not run; reclaim the leaked Arc reference.
        // SAFETY: `arg` came from `CompletionSlot::into_callback_arg` above.
        unsafe {
            let _ = CompletionSlot::from_callback_arg(arg);
        }
        return Err(e);
    }
    Ok(())
}

/// Forward-callback. Runs on the progress thread inside
/// `HG_Trigger`. Recovers the slot, reads the response status,
/// and lets the handle drop to destroy it.
unsafe extern "C" fn forward_cb(info: *const ffi::hg_cb_info) -> ffi::hg_return_t {
    ffi::dispatch_forward_cb(info, |cb| {
        // SAFETY: `cb.arg` came from `CompletionSlot::into_callback_arg`
        // in `forward_one`; reclaim exactly once.
        let slot = unsafe { CompletionSlot::from_callback_arg(cb.arg) };
        // SAFETY: Mercury passes ownership of the handle to the forward
        // callback; the wrapper destroys it when dropped.
        let handle = unsafe { Handle::from_raw(cb.handle) };

        let outcome: HgResult<()> = if cb.ret != ffi::HG_SUCCESS {
            Err(HgError::new(cb.ret as i32, "forward returned error"))
        } else {
            match handle.get_output(BulkGetOut::zeroed()) {
                Ok(out) => {
                    let status = out.status;
                    if status == 0 {
                        Ok(())
                    } else {
                        Err(HgError::new(status, "server reported error"))
                    }
                }
                Err(e) => Err(e),
            }
        };
        slot.complete(outcome);
        // `handle` drops here -> `HG_Destroy`.
        ffi::HG_SUCCESS
    })
}
