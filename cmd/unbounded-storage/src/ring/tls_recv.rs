// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Record-aware receive operations for kernel-TLS sockets, plus the
//! readiness poll that drives a non-blocking TLS handshake.
//!
//! These live apart from the plaintext socket ops in `network.rs` for
//! two reasons: they form one coherent concern (kTLS-aware I/O), and
//! keeping them here holds `network.rs` under the file-size cap. They
//! are `impl NetHandle` methods, so callers reach them through the same
//! handle as the plaintext ops.
//!
//! On a kTLS receive socket a plain `recv` returns `-EIO` whenever a
//! non-`application_data` TLS record (a post-handshake ticket, a key
//! update, or an alert) sits at the head of the stream. `recvmsg` with
//! a control buffer instead surfaces the record type, letting the caller
//! skip control records and detect `close_notify`. Two destinations are
//! supported: a heap buffer (`recv_msg`, for response headers) and the
//! registered backing at buffer index 0 (`recv_fixed_msg`, the
//! zero-copy body path).

use std::future::Future;
use std::io;
use std::os::fd::RawFd;
use std::rc::Rc;

use io_uring::{opcode, types};

use super::core::{OpResource, RecvMsgState, check_res};
use super::network::{NetHandle, OwnedNetFut, OwnedSubmitSlot, RecvRecord, TLS_RECORD_TYPE_ALERT};

impl NetHandle {
    /// Wait for readiness on `fd` and return the reported `revents`.
    /// `flags` is a `poll(2)` event mask (`POLLIN`/`POLLOUT`/...). Used
    /// to drive a non-blocking TLS handshake whose `SSL_connect` reports
    /// `WANT_READ`/`WANT_WRITE`, without ever reading socket bytes (which
    /// on a mid-handshake kTLS socket would be incorrect).
    pub fn poll_ready(
        &self,
        fd: RawFd,
        flags: u32,
    ) -> impl Future<Output = io::Result<u32>> + 'static {
        let ring = Rc::clone(self.ring_cell());
        async move {
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot) = {
                let r = ring.as_ref();
                let ud = r.core.alloc_user_data();
                let sqe = opcode::PollAdd::new(types::Fd(fd), flags)
                    .build()
                    .user_data(ud);
                // SAFETY: poll references no caller memory.
                let slot = r.core.submit_now(sqe, false, OpResource::None)?;
                (ud, slot)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot).await;
            check_res(res).map(|n| n as u32)
        }
    }

    /// `recvmsg` into a fresh heap buffer (up to `max_len` bytes) that
    /// also reports the TLS record type. Used for the response-header
    /// read on a kTLS connection, where a plain `recv` would fail on a
    /// control record. Returns the received bytes and the record type
    /// (`23` = `application_data` when no control message is present).
    pub fn recv_msg(
        &self,
        fd: RawFd,
        max_len: usize,
    ) -> impl Future<Output = io::Result<(Vec<u8>, u8)>> + 'static {
        let ring = Rc::clone(self.ring_cell());
        async move {
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot, state) = {
                let r = ring.as_ref();
                let state = RecvMsgState::new_heap(max_len);
                let msg_ptr = state.borrow().msghdr_ptr();
                let ud = r.core.alloc_user_data();
                let sqe = opcode::RecvMsg::new(types::Fd(fd), msg_ptr)
                    .build()
                    .user_data(ud);
                // SAFETY: `msg_ptr` addresses the `msghdr` owned by
                // `state` (held alive by the ring via OpResource until
                // the CQE is reaped); its iovec base points at the heap
                // buffer inside the same `state`.
                let slot = r
                    .core
                    .submit_now(sqe, false, OpResource::RecvMsg(Rc::clone(&state)))?;
                (ud, slot, state)
            };
            // Heap destination: no fixed-buffer quarantine needed.
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot).await;
            let n = check_res(res)? as usize;
            let record_type = state.borrow().record_type();
            let data = state.borrow().take_data(n);
            Ok((data, record_type))
        }
    }

    /// `recvmsg` counterpart of [`NetHandle::recv_fixed`] that also
    /// reports the TLS record type via a control message. The body lands
    /// directly in the registered backing at `page_byte_offset` (buf
    /// index 0), preserving the zero-copy receive; the extra control
    /// buffer lets a kTLS reader distinguish `application_data` from
    /// post-handshake/alert records that a plain `recv` would surface as
    /// `-EIO`. The same recv-quarantine drop contract as `recv_fixed`
    /// applies (see that method).
    pub fn recv_fixed_msg(
        &self,
        fd: RawFd,
        page_byte_offset: usize,
        len: usize,
    ) -> impl Future<Output = io::Result<RecvRecord>> + 'static {
        let ring = Rc::clone(self.ring_cell());
        async move {
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot, state) = {
                let r = ring.as_ref();
                let ptr = r.fixed_ptr(0, page_byte_offset)?;
                let state = RecvMsgState::new(ptr as *mut u8, len);
                let msg_ptr = state.borrow().msghdr_ptr();
                let ud = r.core.alloc_user_data();
                let sqe = opcode::RecvMsg::new(types::Fd(fd), msg_ptr)
                    .build()
                    .user_data(ud);
                // SAFETY: `msg_ptr` addresses the `msghdr` owned by
                // `state` (held by the ring via OpResource until the CQE
                // is reaped); its iovec base points inside the registered
                // backing, under the same quarantine contract as
                // `recv_fixed`.
                let slot = r
                    .core
                    .submit_now(sqe, false, OpResource::RecvMsg(Rc::clone(&state)))?;
                (ud, slot, state)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot)
                .fixed_recv(page_byte_offset)
                .await;
            let n = check_res(res)? as usize;
            let record_type = state.borrow().record_type();
            // An alert landed its two-byte payload in the page; read the
            // description byte back so the caller can distinguish a
            // graceful close_notify from a fatal alert. The op has
            // completed, so the kernel-written bytes are stable.
            let alert_desc = if record_type == TLS_RECORD_TYPE_ALERT && n >= 2 {
                let ptr = ring.fixed_ptr(0, page_byte_offset)?;
                // SAFETY: `ptr` addresses the registered page the kernel
                // just decrypted at least two alert bytes into.
                Some(unsafe { *ptr.add(1) })
            } else {
                None
            };
            Ok(RecvRecord {
                len: n,
                record_type,
                alert_desc,
            })
        }
    }
}
