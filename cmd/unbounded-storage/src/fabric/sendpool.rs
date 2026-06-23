// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-fabric pre-registered send slab for the `FI_EP_MSG` transport.
//!
//! Both send hot paths (the client request send and the server reply
//! send) frame a small control message and hand it to `fi_send`.
//! Providers that negotiate `FI_MR_LOCAL` (verbs) require the send
//! buffer to carry a `desc` from a registered region. Registering a
//! fresh `LocalMr` per message works correctly but serializes every
//! send through the verbs domain lock (`fi_mr_reg`/`fi_close` on each
//! op), which deadlocks under high concurrency and caps throughput far
//! below the hardware ceiling regardless.
//!
//! This pool registers one domain-wide slab of fixed-size slots up
//! front. A send copies its framed bytes into a free slot and posts
//! with the slab's pre-registered `desc`; the completion handler
//! returns the slot to the free list. Because local MRs are
//! domain-scoped, a single slab serves every connection's endpoint.
//!
//! Fallbacks keep correctness without the slab: providers without
//! `FI_MR_LOCAL` (tcp) skip the slab entirely and post `desc = NULL`,
//! and a message larger than a slot, or a momentarily exhausted free
//! list, falls back to a transient per-message `LocalMr` (the previous
//! behavior). The common path - small control frames under normal
//! concurrency - never touches the domain lock.

use std::ffi::c_void;
use std::ptr;
use std::slice;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use super::backing::LocalMrCtx;
use super::completion::{CompletionFuture, CompletionRegistry, CompletionSlot};
use super::error::{FabricError, Result, check};
use super::ffi;
use super::wire::RECV_BUF_LEN;

/// Size of one send slot. Control frames are well under this; oversize
/// messages fall back to a transient registration.
const SEND_SLOT_LEN: usize = RECV_BUF_LEN;

/// Number of pre-registered slots in the slab. The transient fallback
/// covers momentary exhaustion, so this need not match `max_inflight`.
const SEND_SLAB_DEPTH: usize = 256;

/// A domain-wide pool of pre-registered send buffers shared by every
/// connection on the fabric.
pub(crate) struct SendPool {
    completions: Arc<CompletionRegistry>,
    /// Used for tcp (returns `None` -> `desc = NULL`) and as the verbs
    /// fallback when the slab is exhausted or the message is oversize.
    local_ctx: LocalMrCtx,
    slab: Option<SendSlab>,
}

struct SendSlab {
    /// Owns the slab allocation for the fabric's lifetime; `base`
    /// points into it.
    _buf: Box<[u8]>,
    base: *mut u8,
    mr: *mut ffi::fid_mr,
    desc: *mut c_void,
    free: Arc<Mutex<Vec<usize>>>,
}

// SAFETY: the slab is a flat heap region with a libfabric MR handle;
// access is synchronized by the free-list mutex and the completion
// machinery, and the raw pointers are never aliased mutably across
// threads outside that discipline.
unsafe impl Send for SendSlab {}
unsafe impl Sync for SendSlab {}

impl SendPool {
    /// Build the pool. On verbs (`needs_local`) this registers the slab
    /// MR against `domain`; on tcp it leaves the slab unset and every
    /// send posts `desc = NULL`.
    pub(crate) fn new(
        domain: *mut ffi::fid_domain,
        needs_local: bool,
        next_mr_key: Arc<AtomicU64>,
        completions: Arc<CompletionRegistry>,
    ) -> Result<Self> {
        let local_ctx = LocalMrCtx::new(domain, needs_local, Arc::clone(&next_mr_key));
        if !needs_local {
            return Ok(Self {
                completions,
                local_ctx,
                slab: None,
            });
        }

        let mut buf = vec![0u8; SEND_SLOT_LEN * SEND_SLAB_DEPTH].into_boxed_slice();
        let base = buf.as_mut_ptr();
        let key = next_mr_key.fetch_add(1, Ordering::Relaxed);
        let mut mr: *mut ffi::fid_mr = ptr::null_mut();
        // SAFETY: `base`/`buf.len()` describe a live heap region held by
        // `buf` for the slab's lifetime; `mr` is an out-param.
        let rc = unsafe {
            ffi::ub_fi_mr_reg(
                domain,
                base as *const c_void,
                buf.len(),
                ffi::FI_TRANSMIT,
                0,
                key,
                0,
                &mut mr,
                ptr::null_mut(),
            )
        };
        check("fi_mr_reg(send slab)", rc)?;
        // SAFETY: registration succeeded, so `mr` is a valid handle.
        let desc = unsafe { ffi::ub_fi_mr_desc(mr) };
        let free = Arc::new(Mutex::new((0..SEND_SLAB_DEPTH).collect::<Vec<_>>()));

        Ok(Self {
            completions,
            local_ctx,
            slab: Some(SendSlab {
                _buf: buf,
                base,
                mr,
                desc,
                free,
            }),
        })
    }

    /// The slab MR handle, if any, so the fabric can close it in its
    /// ordered teardown alongside the other domain MRs. The slab itself
    /// never closes the handle.
    pub(crate) fn slab_mr(&self) -> Option<*mut ffi::fid_mr> {
        self.slab.as_ref().map(|s| s.mr)
    }

    /// Frame-send `framed` on `ep`, returning a future for the send
    /// completion. Uses a pre-registered slot when one is available;
    /// otherwise falls back to a transient per-message registration.
    pub(crate) fn send_framed(
        &self,
        ep: *mut ffi::fid_ep,
        framed: Vec<u8>,
        label: &'static str,
        backoff: Duration,
    ) -> Result<CompletionFuture> {
        if let Some(slab) = &self.slab {
            if framed.len() <= SEND_SLOT_LEN {
                let idx = slab.free.lock().unwrap().pop();
                if let Some(idx) = idx {
                    return self.send_via_slot(ep, slab, idx, framed, label, backoff);
                }
            }
        }
        self.send_via_owned(ep, framed, label, backoff)
    }

    fn send_via_slot(
        &self,
        ep: *mut ffi::fid_ep,
        slab: &SendSlab,
        idx: usize,
        framed: Vec<u8>,
        label: &'static str,
        backoff: Duration,
    ) -> Result<CompletionFuture> {
        let len = framed.len();
        // SAFETY: `idx` came from the free list so it is in range, and
        // the slot region [idx*LEN, idx*LEN+len) is disjoint from every
        // other in-flight slot; `len <= SEND_SLOT_LEN`.
        let slot_ptr = unsafe { slab.base.add(idx * SEND_SLOT_LEN) };
        // SAFETY: source and destination are valid for `len` bytes and
        // do not overlap.
        unsafe { ptr::copy_nonoverlapping(framed.as_ptr(), slot_ptr, len) };

        let (slot, fut) = match self.completions.allocate() {
            Ok(pair) => pair,
            Err(e) => {
                slab.free.lock().unwrap().push(idx);
                return Err(e);
            }
        };
        let free = Arc::clone(&slab.free);
        slot.set_handler(move |_| {
            free.lock().unwrap().push(idx);
        });
        let ctx = slot.into_raw();
        let rc = post_send(ep, slot_ptr as *const c_void, len, slab.desc, ctx, backoff);
        if rc < 0 {
            // SAFETY: `ctx` was just produced by `into_raw` and not yet
            // reclaimed; libfabric raised no completion for it.
            let _ = unsafe { CompletionSlot::from_raw(ctx) };
            slab.free.lock().unwrap().push(idx);
            return Err(FabricError::Pkg(label, rc as i32));
        }
        Ok(fut)
    }

    fn send_via_owned(
        &self,
        ep: *mut ffi::fid_ep,
        framed: Vec<u8>,
        label: &'static str,
        backoff: Duration,
    ) -> Result<CompletionFuture> {
        let len = framed.len();
        let buf_ptr = Box::into_raw(framed.into_boxed_slice());
        let buf_addr = buf_ptr as *mut u8 as usize;
        let local_mr = self
            .local_ctx
            .register(buf_ptr as *mut c_void, len, ffi::FI_TRANSMIT)?;
        let desc = local_mr
            .as_ref()
            .map(|m| m.desc())
            .unwrap_or(ptr::null_mut());

        let (slot, fut) = match self.completions.allocate() {
            Ok(pair) => pair,
            Err(e) => {
                // SAFETY: `buf_addr`/`len` came from `Box::into_raw` on a
                // `Box<[u8]>` and ownership was not transferred.
                let _ = unsafe {
                    Box::from_raw(slice::from_raw_parts_mut(buf_addr as *mut u8, len) as *mut [u8])
                };
                return Err(e);
            }
        };
        slot.set_handler(move |_| {
            let _local_mr = &local_mr;
            // SAFETY: same provenance as above; libfabric returns
            // ownership exactly once on completion.
            let _ = unsafe {
                Box::from_raw(slice::from_raw_parts_mut(buf_addr as *mut u8, len) as *mut [u8])
            };
        });
        let ctx = slot.into_raw();
        let rc = post_send(ep, buf_ptr as *const c_void, len, desc, ctx, backoff);
        if rc < 0 {
            // SAFETY: `ctx` was just produced by `into_raw`; no
            // completion was raised, so reclaim it and the buffer here.
            let _ = unsafe { CompletionSlot::from_raw(ctx) };
            let _ = unsafe {
                Box::from_raw(slice::from_raw_parts_mut(buf_ptr as *mut u8, len) as *mut [u8])
            };
            return Err(FabricError::Pkg(label, rc as i32));
        }
        Ok(fut)
    }
}

/// Post one `fi_send`, retrying on `-FI_EAGAIN` (transmit queue briefly
/// full) until the operation is accepted or `backoff` elapses. EAGAIN
/// consumes neither the buffer nor the context, so retrying the same
/// arguments is sound.
fn post_send(
    ep: *mut ffi::fid_ep,
    ptr: *const c_void,
    len: usize,
    desc: *mut c_void,
    ctx: *mut c_void,
    backoff: Duration,
) -> isize {
    let deadline = Instant::now() + backoff;
    loop {
        // SAFETY: ep, buffer, desc, and ctx all remain live for the
        // call; on `-FI_EAGAIN` libfabric neither consumes the buffer
        // nor generates a completion, so reuse on retry is sound.
        let rc = unsafe { ffi::ub_fi_send(ep, ptr, len, desc, ffi::FI_ADDR_UNSPEC, ctx) };
        if rc as i32 != -ffi::FI_EAGAIN || Instant::now() >= deadline {
            break rc;
        }
        thread::sleep(Duration::from_millis(1));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A tcp-mode pool (no slab) posting on a null endpoint must surface
    /// the `fi_send` error and free its owned buffer rather than leak or
    /// hang. `LocalMrCtx` with `needs = false` returns `None`, so the
    /// send posts `desc = NULL` through the owned path.
    #[test]
    fn send_on_null_ep_errors_without_leaking_tcp_mode() {
        let pool = SendPool {
            completions: CompletionRegistry::new(8),
            local_ctx: LocalMrCtx::none(),
            slab: None,
        };
        let res = pool.send_framed(
            ptr::null_mut(),
            vec![1u8, 2, 3, 4],
            "fi_send (test)",
            Duration::from_millis(0),
        );
        assert!(res.is_err(), "send on null ep should error");
    }
}
