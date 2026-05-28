// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Tagged ping/pong over the fabric.
//!
//! Design:
//!
//! * Both sides reserve two tags: [`PING_TAG`] and [`PONG_TAG`].
//! * Each `Fabric` keeps a long-running *ping responder* that holds
//!   a recv buffer and a send buffer. On startup it posts a single
//!   `fi_trecv(tag=PING_TAG, src=FI_ADDR_UNSPEC)`. When that recv
//!   completes, a slot handler runs on the progress thread; it
//!   copies the nonce into the send buffer, posts `fi_tsend(
//!   tag=PONG_TAG, dest=src_addr_from_completion)`, then re-arms
//!   the recv with a fresh `CompletionSlot`.
//! * `Fabric::ping` posts a recv with `tag=PONG_TAG` from the target
//!   peer, then a send with `tag=PING_TAG`. The recv completion's
//!   nonce should match what was sent. The first end-to-end latency
//!   sample is the recv future's wait.
//!
//! The `src_addr` field on the completion is populated by the
//! progress loop via `fi_cq_readfrom`, which requires the sending
//! peer to already be present in the receiver's AV; for the loopback
//! test the test calls `add_connection` in both directions before
//! pinging.

use std::ptr;
use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::bufferpool::PeerId;

use super::completion::CompletionInfo;
use super::error::{FabricError, Result};
use super::fabric::{Fabric, FabricInner};
use super::ffi;

/// Reserved tag for ping packets. Chosen near the top of u64 to avoid
/// colliding with RPC tags that later phases allocate from the
/// low/middle range.
pub const PING_TAG: u64 = 0xFFFF_FFFF_FFFF_FFFE;
/// Reserved tag for pong packets.
pub const PONG_TAG: u64 = 0xFFFF_FFFF_FFFF_FFFD;

/// Payload size for one ping/pong message. We use a u64 nonce only.
const PAYLOAD_LEN: usize = 8;

impl Fabric {
    /// Send a ping to `peer` and wait for the matching pong. Returns
    /// the round-trip latency. On `timeout`, returns
    /// [`FabricError::Timeout`].
    pub fn ping(&self, peer: PeerId, timeout: Duration) -> Result<Duration> {
        let fi_addr = self.lookup_fi_addr(peer)?;
        let inner = self.inner();

        // Allocate recv buffer + slot first so the receive is posted
        // before our send races with the peer's pong.
        let recv_buf: Box<[u8; PAYLOAD_LEN]> = Box::new([0u8; PAYLOAD_LEN]);
        let recv_buf_ptr = Box::into_raw(recv_buf);
        let (recv_slot, recv_fut) = inner.completions.allocate()?;
        let recv_ctx = recv_slot.into_raw();

        // SAFETY: ep, recv_buf_ptr, recv_ctx are all live for the
        // duration of the call.
        let rc = unsafe {
            ffi::ub_fi_trecv(
                inner.ep(),
                recv_buf_ptr as *mut std::ffi::c_void,
                PAYLOAD_LEN,
                ptr::null_mut(),
                fi_addr,
                PONG_TAG,
                0,
                recv_ctx,
            )
        };
        if rc < 0 {
            // Reclaim the slot so we don't leak; libfabric never took
            // ownership.
            // SAFETY: we just handed out `recv_ctx` from `into_raw`.
            let _ = unsafe { super::completion::CompletionSlot::from_raw(recv_ctx) };
            // SAFETY: same for the buffer.
            let _ = unsafe { Box::from_raw(recv_buf_ptr) };
            return Err(FabricError::Pkg("fi_trecv (ping)", rc as i32));
        }

        // Build and post the ping.
        let nonce: u64 = (Instant::now().elapsed().as_nanos() as u64) ^ rand_nonce();
        let send_buf: Box<[u8; PAYLOAD_LEN]> = Box::new(nonce.to_le_bytes());
        let send_buf_ptr = Box::into_raw(send_buf);
        let (send_slot, _send_fut) = inner.completions.allocate()?;
        // The send completion does not need to drive anything; install
        // a handler that frees the heap buffer when libfabric returns
        // the slot.
        let send_buf_for_drop = send_buf_ptr as usize;
        send_slot.set_handler(move |_| {
            // SAFETY: we allocated `send_buf_for_drop` via Box::into_raw
            // immediately above; libfabric returns the same context.
            let _ = unsafe { Box::from_raw(send_buf_for_drop as *mut [u8; PAYLOAD_LEN]) };
        });
        let send_ctx = send_slot.into_raw();

        let started = Instant::now();
        // SAFETY: ep / buffer / context all live for the call.
        let rc = unsafe {
            ffi::ub_fi_tsend(
                inner.ep(),
                send_buf_ptr as *const std::ffi::c_void,
                PAYLOAD_LEN,
                ptr::null_mut(),
                fi_addr,
                PING_TAG,
                send_ctx,
            )
        };
        if rc < 0 {
            // SAFETY: reclaim and drop the slot we just minted.
            let _ = unsafe { super::completion::CompletionSlot::from_raw(send_ctx) };
            // SAFETY: same for the send buffer.
            let _ = unsafe { Box::from_raw(send_buf_ptr) };
            // Best-effort: leave the recv posted; it will time out or
            // be cancelled at fabric drop. The recv buffer is freed
            // by the recv handler we install below, so install it now.
            // For now we leak the recv buffer + slot until shutdown;
            // documenting as a known limitation for Phase 4.
            return Err(FabricError::Pkg("fi_tsend (ping)", rc as i32));
        }

        // Wait for the pong on a noop-waker block_on with a wallclock
        // timeout. The recv buffer must be freed before we return.
        let outcome = block_on_with_timeout(recv_fut, timeout);
        // SAFETY: we allocated the recv buffer via Box::into_raw and
        // libfabric is done with it once the completion arrived.
        let _ = unsafe { Box::from_raw(recv_buf_ptr) };
        let _info: CompletionInfo = outcome?;
        Ok(started.elapsed())
    }
}

/// Install the ping responder against `fabric`. Called from
/// `Fabric::new` after the EP is enabled.
pub(crate) fn install_ping_responder(fabric: &Fabric) -> Result<()> {
    post_responder_recv(fabric.inner_arc())
}

/// Post a recv for the next ping. Allocates a fresh buffer + slot;
/// installs a handler that, on completion, emits a pong and re-arms.
fn post_responder_recv(inner: Arc<FabricInner>) -> Result<()> {
    let buf: Box<[u8; PAYLOAD_LEN]> = Box::new([0u8; PAYLOAD_LEN]);
    let buf_ptr = Box::into_raw(buf);
    let (slot, _fut) = inner.completions.allocate()?;
    let inner_for_handler = inner.clone();
    let buf_addr = buf_ptr as usize;
    slot.set_handler(move |result| {
        // SAFETY: buf_addr was produced by Box::into_raw above; the
        // progress thread runs this handler exactly once.
        let recv_buf: Box<[u8; PAYLOAD_LEN]> =
            unsafe { Box::from_raw(buf_addr as *mut [u8; PAYLOAD_LEN]) };
        match result {
            Ok(info) => {
                // Echo back as pong. Best-effort: if the sender's
                // address was not captured (e.g. provider does not
                // populate it via fi_cq_readfrom) we cannot reply.
                if info.src_addr != ffi::FI_ADDR_UNSPEC {
                    let _ = emit_pong(inner_for_handler.clone(), info.src_addr, *recv_buf);
                }
            }
            Err(_) => {
                // Recv errored (likely fabric shutdown). Do not
                // re-arm; the fabric is going away.
                return;
            }
        }
        // Re-arm for the next ping. On failure we silently stop the
        // responder; the next ping from anyone would simply not be
        // answered, surfacing as a Timeout on the caller side.
        let _ = post_responder_recv(inner_for_handler);
    });
    let ctx = slot.into_raw();
    // SAFETY: ep, buf_ptr, ctx all live for the call. The handler
    // will free the buffer; the slot's CompletionSlot::Drop releases
    // libfabric's reference in progress_loop.
    let rc = unsafe {
        ffi::ub_fi_trecv(
            inner.ep(),
            buf_ptr as *mut std::ffi::c_void,
            PAYLOAD_LEN,
            ptr::null_mut(),
            ffi::FI_ADDR_UNSPEC,
            PING_TAG,
            0,
            ctx,
        )
    };
    if rc < 0 {
        // Reclaim slot + buffer so we don't leak.
        // SAFETY: ctx was produced by `into_raw` immediately above.
        let _ = unsafe { super::completion::CompletionSlot::from_raw(ctx) };
        // SAFETY: same for the buffer.
        let _ = unsafe { Box::from_raw(buf_ptr) };
        return Err(FabricError::Pkg("fi_trecv (responder)", rc as i32));
    }
    Ok(())
}

fn emit_pong(
    inner: Arc<FabricInner>,
    dest: ffi::fi_addr_t,
    payload: [u8; PAYLOAD_LEN],
) -> Result<()> {
    let send_buf: Box<[u8; PAYLOAD_LEN]> = Box::new(payload);
    let send_buf_ptr = Box::into_raw(send_buf);
    let (slot, _fut) = inner.completions.allocate()?;
    let buf_addr = send_buf_ptr as usize;
    slot.set_handler(move |_| {
        // SAFETY: ownership returned exactly once.
        let _ = unsafe { Box::from_raw(buf_addr as *mut [u8; PAYLOAD_LEN]) };
    });
    let ctx = slot.into_raw();
    // SAFETY: ep, buffer, ctx all live for the call.
    let rc = unsafe {
        ffi::ub_fi_tsend(
            inner.ep(),
            send_buf_ptr as *const std::ffi::c_void,
            PAYLOAD_LEN,
            ptr::null_mut(),
            dest,
            PONG_TAG,
            ctx,
        )
    };
    if rc < 0 {
        // SAFETY: reclaim slot + buffer.
        let _ = unsafe { super::completion::CompletionSlot::from_raw(ctx) };
        let _ = unsafe { Box::from_raw(send_buf_ptr) };
        return Err(FabricError::Pkg("fi_tsend (pong)", rc as i32));
    }
    Ok(())
}

/// Spin-poll a future to completion with a wallclock timeout. Used
/// only by [`Fabric::ping`]; the rest of the crate's futures are
/// driven by an embedder-provided executor.
fn block_on_with_timeout<F>(mut fut: F, timeout: Duration) -> Result<CompletionInfo>
where
    F: std::future::Future<Output = Result<CompletionInfo>>,
{
    use std::pin::Pin;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    fn raw() -> RawWaker {
        static VT: RawWakerVTable =
            RawWakerVTable::new(|_| RawWaker::new(ptr::null(), &VT), |_| {}, |_| {}, |_| {});
        RawWaker::new(ptr::null(), &VT)
    }
    // SAFETY: vtable never dereferences the data pointer.
    let waker = unsafe { Waker::from_raw(raw()) };
    let mut cx = Context::from_waker(&waker);
    // SAFETY: we own `fut` by value on the stack.
    let mut fut = unsafe { Pin::new_unchecked(&mut fut) };
    let started = Instant::now();
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

/// Tiny seedless nonce. Not security-relevant; only used to make the
/// ping payload non-zero.
fn rand_nonce() -> u64 {
    use std::collections::hash_map::DefaultHasher;
    use std::hash::Hasher;
    let mut h = DefaultHasher::new();
    h.write_u128(Instant::now().elapsed().as_nanos());
    h.write_usize(std::process::id() as usize);
    h.finish()
}
