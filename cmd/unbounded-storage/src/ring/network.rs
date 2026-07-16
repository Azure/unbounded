// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Socket I/O facade over [`RingCore`].
//!
//! [`NetworkRing`] drives connect / accept / recv / send over sockets
//! the shard creates with plain `libc` (this ring never creates fds; it
//! only performs I/O on already-open ones). It mirrors the disk ring's
//! `SINGLE_ISSUER | DEFER_TASKRUN` choices but omits `IOPOLL` (the
//! kernel rejects it for socket ops), and falls back to a plain ring if
//! the kernel rejects those flags.
//!
//! [`SockAddr`] is an owned wrapper around a `libc::sockaddr_storage` so
//! `connect` can hand the kernel a stable pointer for the op's lifetime.
//!
//! [`NetHandle`] is the cloneable `Rc`-based serving handle: its methods
//! return `'static` futures (they own a ring clone instead of borrowing
//! it) so the serving frontend can store many per-connection futures
//! across shard-loop ticks. They still park on [`RingCore`]'s submission
//! backpressure before pushing SQEs. The borrowed [`NetworkRing`] methods
//! return `'_` futures for callers that keep the ring on the stack.
//!
//! ## SEND_ZC two-CQE semantics
//!
//! `send_zc_fixed` submits with `expects_more = true`. The op resolves
//! only on the SEND_ZC notification CQE (see [`RingCore`] docs), so when
//! the future returns the kernel is done with the source page. If a
//! fixed-source send future is dropped first, the ring either quarantines
//! the pool page until that notification is reaped, or holds an explicit
//! completion resource for cross-shard remote pins.

use std::cell::RefCell;
use std::future::Future;
use std::io;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use io_uring::{opcode, types};

use super::core::{
    OpFut, OpResource, RecvQuarantine, RingCore, RingSetup, SendCompletion, Slot, SubmitSlot,
    check_res,
};

/// TLS `application_data` record type (RFC 8446). Plaintext bytes the
/// kernel hands back as ordinary payload also default to this.
pub const TLS_RECORD_TYPE_APPLICATION_DATA: u8 = 23;
/// TLS `alert` record type (RFC 8446). A `close_notify` alert arrives as
/// one of these and signals orderly stream end.
pub const TLS_RECORD_TYPE_ALERT: u8 = 21;
/// TLS `handshake` record type (RFC 8446). Post-handshake messages
/// (`NewSessionTicket`, `KeyUpdate`) surface as these on a kTLS RX
/// socket and must be skipped by the application.
pub const TLS_RECORD_TYPE_HANDSHAKE: u8 = 22;

/// Outcome of a [`NetHandle::recv_fixed_msg`]: how many bytes landed in
/// the registered destination and the TLS record type the kernel
/// reported for them (defaulting to `application_data` on a plaintext
/// socket).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct RecvRecord {
    /// Bytes written into the registered destination page.
    pub len: usize,
    /// TLS content type of the record (`application_data` on plaintext).
    pub record_type: u8,
    /// For an `alert` record, the alert description byte (the second of
    /// the alert's two payload bytes), so a caller can tell a graceful
    /// `close_notify` from a fatal alert that truncated the stream.
    /// `None` for non-alert records or a malformed (sub-2-byte) alert.
    pub alert_desc: Option<u8>,
}

/// Thin owned wrapper around a `libc::sockaddr` plus its length, so
/// `connect` can hand a stable pointer to the kernel for the op's
/// duration.
///
/// `Clone`/`Copy` because the stored `sockaddr_storage` is plain bytes:
/// an origin backend clones the resolved origin address into each
/// self-owned fetch future so the produced page stream borrows nothing
/// from the backend, which lets the backend live behind a hot-swappable
/// registry (see [`crate::backend::BackendRegistry`]).
#[derive(Clone, Copy)]
pub struct SockAddr {
    storage: libc::sockaddr_storage,
    len: libc::socklen_t,
}

impl SockAddr {
    /// Build from a raw `sockaddr` and its length. The bytes are copied
    /// into owned storage so the caller need not keep the source alive.
    ///
    /// # Safety
    ///
    /// `addr` must point to at least `len` valid bytes describing a
    /// `sockaddr` of a family the kernel understands.
    pub unsafe fn from_raw(addr: *const libc::sockaddr, len: libc::socklen_t) -> Self {
        let mut storage: libc::sockaddr_storage = unsafe { std::mem::zeroed() };
        let n = (len as usize).min(std::mem::size_of::<libc::sockaddr_storage>());
        unsafe {
            std::ptr::copy_nonoverlapping(
                addr as *const u8,
                &mut storage as *mut libc::sockaddr_storage as *mut u8,
                n,
            );
        }
        Self { storage, len }
    }

    /// Build from a `libc::sockaddr_in` (IPv4).
    pub fn from_sockaddr_in(addr: libc::sockaddr_in) -> Self {
        // SAFETY: sockaddr_in fits inside sockaddr_storage and the
        // length is exactly its size.
        unsafe {
            Self::from_raw(
                &addr as *const libc::sockaddr_in as *const libc::sockaddr,
                std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t,
            )
        }
    }

    /// Duplicate by copying the owned storage. Used by the serving
    /// handle so each `connect` can hand the ring a fresh owned copy.
    pub fn duplicate(&self) -> Self {
        Self {
            storage: self.storage,
            len: self.len,
        }
    }

    /// Decode as an IPv4 `(addr, port)` pair, if it holds an `AF_INET`
    /// `sockaddr_in`. Returns `None` for any non-IPv4 family.
    pub fn as_ipv4(&self) -> Option<(std::net::Ipv4Addr, u16)> {
        if self.storage.ss_family != libc::AF_INET as libc::sa_family_t {
            return None;
        }
        // SAFETY: ss_family is AF_INET, so the storage holds a valid
        // sockaddr_in; reading it back out is sound.
        let sin: libc::sockaddr_in = unsafe {
            std::ptr::read(
                &self.storage as *const libc::sockaddr_storage as *const libc::sockaddr_in,
            )
        };
        let addr = std::net::Ipv4Addr::from(u32::from_be(sin.sin_addr.s_addr));
        let port = u16::from_be(sin.sin_port);
        Some((addr, port))
    }

    fn as_ptr(&self) -> *const libc::sockaddr {
        &self.storage as *const libc::sockaddr_storage as *const libc::sockaddr
    }

    fn socklen(&self) -> libc::socklen_t {
        self.len
    }
}

/// One io_uring ring dedicated to socket I/O, pinned to the shard
/// thread. See the module docs for the completion / SEND_ZC model.
pub struct NetworkRing {
    pub(crate) core: RingCore,
}

impl NetworkRing {
    /// Build a socket ring with submission/completion depth
    /// `queue_depth`. Uses `SINGLE_ISSUER | DEFER_TASKRUN` (no IOPOLL);
    /// falls back to a plain ring if the kernel rejects those flags.
    pub fn new(queue_depth: u32) -> io::Result<Self> {
        let setup = RingSetup {
            iopoll: false,
            single_issuer: true,
            defer_taskrun: true,
        };
        let core = RingCore::new(queue_depth, setup)?;
        Ok(Self { core })
    }

    /// Register the bufferpool `backing` as fixed buffer index 0, so
    /// SEND_ZC and fixed RECV can target bufferpool pages by offset.
    pub fn register_backing(&self, backing: &crate::memory::Backing) -> io::Result<()> {
        if backing.page_size == 0 || backing.page_count == 0 {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        let len = backing
            .page_size
            .checked_mul(backing.page_count)
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?;
        self.register_region(backing.base, len)
    }

    /// Register a raw `(base, len)` region as fixed buffer index 0, so
    /// SEND_ZC and fixed RECV can target it by offset. This is the
    /// backing-free counterpart of [`Self::register_backing`] for
    /// callers that hold the region as a bare pointer (e.g. a worker's
    /// thread-local scratch backing whose `Backing` Drop carrier cannot
    /// be re-synthesized from raw parts).
    ///
    /// The local shard's own backing must be registered first so it
    /// lands at index 0 (the index `recv_fixed` and the local serving
    /// path assume).
    pub fn register_region(&self, base: *mut u8, len: usize) -> io::Result<()> {
        let idx = self.register_region_indexed(base, len)?;
        debug_assert_eq!(idx, 0, "first region must be fixed buffer index 0");
        Ok(())
    }

    /// Register a raw `(base, len)` region as the next fixed buffer and
    /// return its assigned index. Used to register **peer shards'**
    /// backings (index 1..N) on this shard's socket ring so the
    /// coordinator can SEND_ZC zero-copy directly from an owner shard's
    /// page (cross-shard fan-out). All such regions must be registered
    /// before any I/O is in flight: `RingCore::register_buffer`
    /// unregisters and re-registers the whole fixed-buffer table on each
    /// call, which is only safe while the ring is quiescent.
    pub fn register_region_indexed(&self, base: *mut u8, len: usize) -> io::Result<u16> {
        if base.is_null() || len == 0 {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        self.core.register_buffer(base, len)
    }

    /// Install the sink that defers reuse of a cancelled fixed-buffer
    /// RECV's destination page until its CQE is reaped (see
    /// [`RecvQuarantine`]). Install it before any `recv_fixed` is issued
    /// so every cancelled RECV is covered. Without it, the drop path
    /// falls back to the blocking [`RingCore::cancel_and_drain`].
    pub(crate) fn set_recv_quarantine(&self, q: Rc<dyn RecvQuarantine>) {
        self.core.set_recv_quarantine(q);
    }

    /// Push queued SQEs and drain available CQEs. Returns `true` if any
    /// work happened.
    pub fn progress(&self) -> bool {
        self.core.progress().unwrap_or(false)
    }

    /// Connect `fd` to `addr` (`IORING_OP_CONNECT`). Takes `addr` by
    /// value so the ring owns it for the op's lifetime.
    pub fn connect(&self, fd: RawFd, addr: SockAddr) -> impl Future<Output = io::Result<()>> + '_ {
        async move {
            let addr = Box::new(addr);
            let ud = self.core.alloc_user_data();
            let sqe = opcode::Connect::new(types::Fd(fd), addr.as_ptr(), addr.socklen())
                .build()
                .user_data(ud);
            // SAFETY: the sockaddr pointer addresses `addr`, owned by the
            // ring via OpResource until the op's CQE is reaped.
            let slot = self
                .core
                .submit_op(sqe, false, OpResource::Addr(addr))
                .await?;
            let res = OpFut::new(&self.core, ud, slot).await;
            check_res(res).map(|_| ())
        }
    }

    /// Accept one connection on `listen_fd` (`IORING_OP_ACCEPT`).
    pub fn accept(&self, listen_fd: RawFd) -> impl Future<Output = io::Result<RawFd>> + '_ {
        async move {
            let ud = self.core.alloc_user_data();
            let sqe = opcode::Accept::new(
                types::Fd(listen_fd),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
            )
            .build()
            .user_data(ud);
            // SAFETY: null addr/addrlen: the op references no caller
            // memory.
            let slot = self.core.submit_op(sqe, false, OpResource::None).await?;
            let res = OpFut::new(&self.core, ud, slot).await;
            check_res(res).map(|n| n as RawFd)
        }
    }

    /// Receive up to `max_len` bytes from `fd` into a caller-owned heap
    /// buffer (`IORING_OP_RECV`), returning the bytes read. An empty
    /// `Vec` means the peer closed the connection.
    pub fn recv(
        &self,
        fd: RawFd,
        max_len: usize,
    ) -> impl Future<Output = io::Result<Vec<u8>>> + '_ {
        async move {
            let buf = Rc::new(RefCell::new(vec![0u8; max_len]));
            let ptr = buf.borrow_mut().as_mut_ptr();
            let len = max_len as u32;
            let ud = self.core.alloc_user_data();
            let sqe = opcode::Recv::new(types::Fd(fd), ptr, len)
                .build()
                .user_data(ud);
            // SAFETY: the destination pointer addresses the heap buffer
            // owned by the `Rc` clone handed to OpResource, kept alive by
            // the ring until the op's CQE is reaped.
            let slot = self
                .core
                .submit_op(sqe, false, OpResource::RecvBuf(Rc::clone(&buf)))
                .await?;
            let res = OpFut::new(&self.core, ud, slot).await;
            let n = check_res(res)? as usize;
            let data = buf.borrow()[..n].to_vec();
            Ok(data)
        }
    }

    /// Receive up to `len` bytes from `fd` into the registered fixed
    /// buffer (`buf_index`) at `page_byte_offset` (`IORING_OP_RECV`).
    /// `buf_index` must be 0 (the only registered slot).
    pub fn recv_fixed(
        &self,
        fd: RawFd,
        buf_index: u16,
        page_byte_offset: usize,
        len: usize,
    ) -> impl Future<Output = io::Result<usize>> + '_ {
        async move {
            let ptr = self.fixed_ptr(buf_index, page_byte_offset)?;
            let ud = self.core.alloc_user_data();
            let sqe = opcode::Recv::new(types::Fd(fd), ptr as *mut u8, len as u32)
                .build()
                .user_data(ud);
            // SAFETY: `ptr` points inside the registered backing, kept
            // alive by the embedder for the shard's lifetime.
            let slot = self.core.submit_op(sqe, false, OpResource::None).await?;
            let res = OpFut::new(&self.core, ud, slot).await;
            check_res(res).map(|n| n as usize)
        }
    }

    /// Send `buf` over `fd` from a plain heap buffer
    /// (`IORING_OP_SEND`). Takes `buf` by value so the ring owns it for
    /// the op's lifetime.
    pub fn send(&self, fd: RawFd, buf: Vec<u8>) -> impl Future<Output = io::Result<usize>> + '_ {
        async move {
            let ptr = buf.as_ptr();
            let len = buf.len() as u32;
            let ud = self.core.alloc_user_data();
            let sqe = opcode::Send::new(types::Fd(fd), ptr, len)
                .build()
                .user_data(ud);
            // SAFETY: the source pointer addresses `buf`, owned by the
            // ring via OpResource until the op's CQE is reaped.
            let slot = self
                .core
                .submit_op(sqe, false, OpResource::Buf(buf))
                .await?;
            let res = OpFut::new(&self.core, ud, slot).await;
            check_res(res).map(|n| n as usize)
        }
    }

    /// Zero-copy send `len` bytes from the registered fixed buffer
    /// (`buf_index`) at `page_byte_offset` over `fd`
    /// (`IORING_OP_SEND_ZC`). Resolves only on the final notification
    /// CQE: when this future returns the kernel is done with the source
    /// page.
    pub fn send_zc_fixed(
        &self,
        fd: RawFd,
        buf_index: u16,
        page_byte_offset: usize,
        len: usize,
    ) -> impl Future<Output = io::Result<usize>> + '_ {
        async move {
            if buf_index != 0 {
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            }
            let ptr = self.fixed_ptr(buf_index, page_byte_offset)?;
            let ud = self.core.alloc_user_data();
            let sqe = opcode::SendZc::new(types::Fd(fd), ptr, len as u32)
                .buf_index(Some(buf_index))
                .build()
                .user_data(ud);
            // SAFETY: `ptr` points inside the registered backing; the
            // caller holds the source page until this future resolves.
            // If the future is dropped first, the page is quarantined
            // until the SEND_ZC notification CQE is reaped.
            let slot = self.core.submit_op(sqe, true, OpResource::None).await?;
            let res = OpFut::new(&self.core, ud, slot)
                .quarantine_send_zc(page_byte_offset)
                .await;
            check_res(res).map(|n| n as usize)
        }
    }

    /// SEND_ZC variant that holds `on_complete` until the final
    /// notification CQE is reaped, even if the awaiting future is
    /// dropped first.
    pub(crate) fn send_zc_fixed_with_completion(
        &self,
        fd: RawFd,
        buf_index: u16,
        page_byte_offset: usize,
        len: usize,
        on_complete: Box<dyn SendCompletion>,
    ) -> impl Future<Output = io::Result<usize>> + '_ {
        async move {
            let ptr = self.fixed_ptr(buf_index, page_byte_offset)?;
            let ud = self.core.alloc_user_data();
            let sqe = opcode::SendZc::new(types::Fd(fd), ptr, len as u32)
                .buf_index(Some(buf_index))
                .build()
                .user_data(ud);
            let slot = self
                .core
                .submit_op(sqe, true, OpResource::SendCompletion(on_complete))
                .await?;
            let res = OpFut::new(&self.core, ud, slot).abandon_send_zc().await;
            check_res(res).map(|n| n as usize)
        }
    }

    /// Resolve a `(buf_index, offset)` pair to a raw pointer inside a
    /// registered region. `buf_index` 0 is this shard's own backing;
    /// indices 1..N are peer shards' backings registered via
    /// [`Self::register_region_indexed`] for cross-shard zero-copy send.
    pub(crate) fn fixed_ptr(
        &self,
        buf_index: u16,
        page_byte_offset: usize,
    ) -> io::Result<*const u8> {
        match self.core.registered_base(buf_index) {
            Some(base) => Ok(unsafe { base.as_ptr().add(page_byte_offset) as *const u8 }),
            None => Err(io::Error::from_raw_os_error(libc::EINVAL)),
        }
    }

    /// Count of F_MORE (intermediate SEND_ZC) completions observed.
    /// Test-only, used to assert the two-CQE path executed.
    #[cfg(test)]
    pub(crate) fn more_completions(&self) -> u64 {
        self.core.more_completions()
    }
}

/// A cloneable, `Rc`-based serving handle over a shard's
/// [`NetworkRing`].
///
/// The borrowed futures returned by `NetworkRing`'s methods borrow
/// `&'a NetworkRing`, so they are not `'static`. The serving frontend
/// multiplexes many long-lived per-connection futures the shard loop
/// polls each tick, so it needs `'static` futures. `NetHandle` owns an
/// `Rc<NetworkRing>` clone instead of borrowing the ring: each
/// method parks on the ring's submission backpressure before submitting,
/// then returns a future that owns its own ring clone plus the op's
/// `Rc<Slot>`.
#[derive(Clone)]
pub struct NetHandle {
    ring: Rc<NetworkRing>,
}

impl NetHandle {
    /// Wrap a shared network ring.
    pub fn new(ring: Rc<NetworkRing>) -> Self {
        Self { ring }
    }

    /// Register a peer shard's backing region on this handle's ring and
    /// return its fixed-buffer index, so this shard can SEND_ZC
    /// zero-copy from the peer's pages (cross-shard fan-out). Must be
    /// called during bring-up before any I/O is in flight (see
    /// [`NetworkRing::register_region_indexed`]).
    pub fn register_peer_region(&self, base: *mut u8, len: usize) -> io::Result<u16> {
        self.ring.register_region_indexed(base, len)
    }

    /// Shared access to the underlying ring, for sibling-module ops
    /// (see `tls_recv`) that submit on this handle's ring.
    pub(crate) fn ring_cell(&self) -> &Rc<NetworkRing> {
        &self.ring
    }

    /// True when both handles drive the very same ring allocation
    /// (their `Rc`s share one `NetworkRing`). Used to assert ring
    /// sharing/isolation across threads.
    #[cfg(test)]
    pub fn same_ring(&self, other: &NetHandle) -> bool {
        Rc::ptr_eq(&self.ring, &other.ring)
    }

    /// Address identity of the underlying ring allocation. Two handles
    /// over the same ring return equal values; handles over distinct
    /// rings return distinct ones.
    #[cfg(test)]
    pub fn ring_addr(&self) -> usize {
        Rc::as_ptr(&self.ring) as usize
    }

    /// `'static` counterpart of [`NetworkRing::accept`].
    pub fn accept(&self, listen_fd: RawFd) -> impl Future<Output = io::Result<RawFd>> + 'static {
        let ring = Rc::clone(&self.ring);
        async move {
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot) = {
                let r = ring.as_ref();
                let ud = r.core.alloc_user_data();
                let sqe = opcode::Accept::new(
                    types::Fd(listen_fd),
                    std::ptr::null_mut(),
                    std::ptr::null_mut(),
                )
                .build()
                .user_data(ud);
                // SAFETY: null addr/addrlen: the op references no caller
                // memory.
                let slot = r.core.submit_now(sqe, false, OpResource::None)?;
                (ud, slot)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot).await;
            check_res(res).map(|n| n as RawFd)
        }
    }

    /// `'static` counterpart of [`NetworkRing::connect`].
    pub fn connect(
        &self,
        fd: RawFd,
        addr: SockAddr,
    ) -> impl Future<Output = io::Result<()>> + 'static {
        let ring = Rc::clone(&self.ring);
        async move {
            let addr = Box::new(addr);
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot) = {
                let r = ring.as_ref();
                let ud = r.core.alloc_user_data();
                let sqe = opcode::Connect::new(types::Fd(fd), addr.as_ptr(), addr.socklen())
                    .build()
                    .user_data(ud);
                // SAFETY: the sockaddr pointer addresses `addr`, owned by
                // the ring via OpResource until the CQE is reaped.
                let slot = r.core.submit_now(sqe, false, OpResource::Addr(addr))?;
                (ud, slot)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot).await;
            check_res(res).map(|_| ())
        }
    }

    /// `'static` counterpart of [`NetworkRing::send`].
    pub fn send(
        &self,
        fd: RawFd,
        buf: Vec<u8>,
    ) -> impl Future<Output = io::Result<usize>> + 'static {
        let ring = Rc::clone(&self.ring);
        async move {
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot) = {
                let r = ring.as_ref();
                let ptr = buf.as_ptr();
                let len = buf.len() as u32;
                let ud = r.core.alloc_user_data();
                let sqe = opcode::Send::new(types::Fd(fd), ptr, len)
                    .build()
                    .user_data(ud);
                // SAFETY: the source pointer addresses `buf`, owned by the
                // ring via OpResource until the CQE is reaped.
                let slot = r.core.submit_now(sqe, false, OpResource::Buf(buf))?;
                (ud, slot)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot).await;
            check_res(res).map(|n| n as usize)
        }
    }

    /// `'static` counterpart of [`NetworkRing::recv`].
    pub fn recv(
        &self,
        fd: RawFd,
        max_len: usize,
    ) -> impl Future<Output = io::Result<Vec<u8>>> + 'static {
        let ring = Rc::clone(&self.ring);
        async move {
            let buf = Rc::new(RefCell::new(vec![0u8; max_len]));
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot) = {
                let r = ring.as_ref();
                let ptr = buf.borrow_mut().as_mut_ptr();
                let len = max_len as u32;
                let ud = r.core.alloc_user_data();
                let sqe = opcode::Recv::new(types::Fd(fd), ptr, len)
                    .build()
                    .user_data(ud);
                // SAFETY: the destination pointer addresses the heap buffer
                // owned by the `Rc` clone handed to OpResource, kept alive
                // by the ring until the CQE is reaped.
                let slot = r
                    .core
                    .submit_now(sqe, false, OpResource::RecvBuf(Rc::clone(&buf)))?;
                (ud, slot)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot).await;
            let n = check_res(res)? as usize;
            let data = buf.borrow()[..n].to_vec();
            Ok(data)
        }
    }

    /// `'static` counterpart of [`NetworkRing::send_zc_fixed`]. Resolves
    /// on the SEND_ZC notification CQE.
    pub fn send_zc_fixed(
        &self,
        fd: RawFd,
        buf_index: u16,
        page_byte_offset: usize,
        len: usize,
    ) -> impl Future<Output = io::Result<usize>> + 'static {
        let ring = Rc::clone(&self.ring);
        async move {
            if buf_index != 0 {
                return Err(io::Error::from_raw_os_error(libc::EINVAL));
            }
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot) = {
                let r = ring.as_ref();
                let ptr = r.fixed_ptr(buf_index, page_byte_offset)?;
                let ud = r.core.alloc_user_data();
                let sqe = opcode::SendZc::new(types::Fd(fd), ptr, len as u32)
                    .buf_index(Some(buf_index))
                    .build()
                    .user_data(ud);
                // SAFETY: `ptr` points inside the registered backing; the
                // caller holds the source page until this future resolves.
                // If the future is dropped first, the page is quarantined
                // until the SEND_ZC notification CQE is reaped.
                let slot = r.core.submit_now(sqe, true, OpResource::None)?;
                (ud, slot)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot)
                .quarantine_send_zc(page_byte_offset)
                .await;
            check_res(res).map(|n| n as usize)
        }
    }

    /// `'static` SEND_ZC variant that holds `on_complete` until the
    /// final notification CQE is reaped, even if the future is dropped.
    pub(crate) fn send_zc_fixed_with_completion(
        &self,
        fd: RawFd,
        buf_index: u16,
        page_byte_offset: usize,
        len: usize,
        on_complete: Box<dyn SendCompletion>,
    ) -> impl Future<Output = io::Result<usize>> + 'static {
        let ring = Rc::clone(&self.ring);
        async move {
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot) = {
                let r = ring.as_ref();
                let ptr = r.fixed_ptr(buf_index, page_byte_offset)?;
                let ud = r.core.alloc_user_data();
                let sqe = opcode::SendZc::new(types::Fd(fd), ptr, len as u32)
                    .buf_index(Some(buf_index))
                    .build()
                    .user_data(ud);
                let slot = r
                    .core
                    .submit_now(sqe, true, OpResource::SendCompletion(on_complete))?;
                (ud, slot)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot)
                .abandon_send_zc()
                .await;
            check_res(res).map(|n| n as usize)
        }
    }

    /// `'static` counterpart of [`NetworkRing::recv_fixed`].
    pub fn recv_fixed(
        &self,
        fd: RawFd,
        buf_index: u16,
        page_byte_offset: usize,
        len: usize,
    ) -> impl Future<Output = io::Result<usize>> + 'static {
        let ring = Rc::clone(&self.ring);
        async move {
            OwnedSubmitSlot::new(Rc::clone(&ring)).await;
            let (ud, slot) = {
                let r = ring.as_ref();
                let ptr = r.fixed_ptr(buf_index, page_byte_offset)?;
                let ud = r.core.alloc_user_data();
                let sqe = opcode::Recv::new(types::Fd(fd), ptr as *mut u8, len as u32)
                    .build()
                    .user_data(ud);
                // SAFETY: `ptr` points inside the registered backing; the
                // caller holds the destination page until this future
                // resolves (the RECV completion CQE). If the future is
                // dropped early, `OwnedNetFut::fixed_recv` either hands
                // the destination page to the ring's recv-quarantine sink
                // (held until the RECV CQE is reaped) or, with no sink
                // installed, drains the RECV to completion, so the kernel
                // never writes into the page after the caller reuses it.
                let slot = r.core.submit_now(sqe, false, OpResource::None)?;
                (ud, slot)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot)
                .fixed_recv(page_byte_offset)
                .await;
            check_res(res).map(|n| n as usize)
        }
    }
}

/// `'static` op future for the [`NetHandle`] path. Owns an
/// `Rc<NetworkRing>` clone instead of borrowing the ring, so it can be
/// stored across shard-loop ticks. The owned slot carries the completion
/// state.
pub(crate) struct OwnedNetFut {
    ring: Rc<NetworkRing>,
    user_data: u64,
    slot: Rc<Slot>,
    /// When `Some(offset)`, this future is a fixed-buffer RECV whose
    /// destination begins at byte `offset` in the registered backing.
    /// Dropping it before completion routes through
    /// [`RingCore::cancel_fixed_recv`], which quarantines that page
    /// (non-blocking) or drains the op (blocking fallback) so the kernel
    /// never writes into a page the caller may reuse. `None` for ops
    /// whose memory the ring owns, where a best-effort cancel suffices
    /// unless `abandon_send_zc` is set for fixed-source SEND_ZC.
    fixed_recv_offset: Option<usize>,
    abandon_send_zc: bool,
    /// Source page offset for a pool-managed SEND_ZC. On early drop,
    /// this page is quarantined until the final notification CQE.
    send_zc_source_offset: Option<usize>,
}

pub(crate) struct OwnedSubmitSlot {
    ring: Rc<NetworkRing>,
}

impl OwnedSubmitSlot {
    pub(crate) fn new(ring: Rc<NetworkRing>) -> Self {
        Self { ring }
    }
}

impl Future for OwnedSubmitSlot {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let mut submit = SubmitSlot {
            core: &self.ring.core,
        };
        Pin::new(&mut submit).poll(cx)
    }
}

impl OwnedNetFut {
    pub(crate) fn new(ring: Rc<NetworkRing>, user_data: u64, slot: Rc<Slot>) -> Self {
        Self {
            ring,
            user_data,
            slot,
            fixed_recv_offset: None,
            abandon_send_zc: false,
            send_zc_source_offset: None,
        }
    }

    /// Mark this future as a fixed-buffer RECV writing into the backing
    /// at byte `page_byte_offset`, so dropping it before completion
    /// makes that destination page safe to reuse (see the field docs).
    pub(crate) fn fixed_recv(mut self, page_byte_offset: usize) -> Self {
        self.fixed_recv_offset = Some(page_byte_offset);
        self
    }

    /// Keep the SEND_ZC slot alive after future drop until the final
    /// notification releases the source page. Drop never blocks.
    pub(crate) fn abandon_send_zc(mut self) -> Self {
        self.abandon_send_zc = true;
        self
    }

    /// Mark this future as a fixed-source SEND_ZC whose source page is
    /// pool-managed. Dropping it before completion quarantines that page
    /// until the final notification CQE is reaped.
    pub(crate) fn quarantine_send_zc(mut self, page_byte_offset: usize) -> Self {
        self.abandon_send_zc = true;
        self.send_zc_source_offset = Some(page_byte_offset);
        self
    }
}

impl Future for OwnedNetFut {
    type Output = i32;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<i32> {
        let this = self.get_mut();
        if this.slot.is_done() {
            return Poll::Ready(this.slot.result());
        }
        this.ring.progress();
        if this.slot.is_done() {
            return Poll::Ready(this.slot.result());
        }
        this.slot.set_waker(cx.waker().clone());
        Poll::Pending
    }
}

impl Drop for OwnedNetFut {
    fn drop(&mut self) {
        if !self.slot.is_done() {
            match self.fixed_recv_offset {
                Some(off) => {
                    self.ring
                        .core
                        .cancel_fixed_recv(self.user_data, off, &self.slot);
                }
                None if self.abandon_send_zc => {
                    self.ring.core.abandon_send_zc(
                        self.user_data,
                        self.send_zc_source_offset,
                        &self.slot,
                    );
                }
                None => self.ring.core.cancel(self.user_data),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::pin::Pin;
    use std::task::{Context, Poll};

    use super::*;
    use crate::runtime::noop_waker;

    fn block_on<F: Future>(ring: &NetworkRing, mut fut: Pin<&mut F>) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut spins = 0u32;
        loop {
            if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
                return v;
            }
            ring.progress();
            spins += 1;
            assert!(spins < 5_000_000, "block_on spun without progress");
        }
    }

    fn socketpair() -> Option<(libc::c_int, libc::c_int)> {
        let mut fds = [0 as libc::c_int; 2];
        let rc = unsafe { libc::socketpair(libc::AF_UNIX, libc::SOCK_STREAM, 0, fds.as_mut_ptr()) };
        if rc != 0 {
            None
        } else {
            Some((fds[0], fds[1]))
        }
    }

    /// Build a connected TCP loopback pair on 127.0.0.1. Unlike AF_UNIX
    /// socketpairs, TCP sockets support zerocopy send (SEND_ZC), so this
    /// is required to exercise the F_MORE + notification two-CQE path.
    /// The handshake completes against a listening socket on the same
    /// thread: connect() to loopback returns once the SYN/ACK lands, and
    /// accept() then dequeues the established connection.
    fn tcp_loopback_pair() -> Option<(libc::c_int, libc::c_int)> {
        unsafe {
            let listener = libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0);
            if listener < 0 {
                return None;
            }
            let mut addr: libc::sockaddr_in = std::mem::zeroed();
            addr.sin_family = libc::AF_INET as libc::sa_family_t;
            addr.sin_addr.s_addr = u32::to_be(0x7f00_0001); // 127.0.0.1
            addr.sin_port = 0; // ephemeral
            let alen = std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t;
            if libc::bind(listener, &addr as *const _ as *const libc::sockaddr, alen) != 0 {
                libc::close(listener);
                return None;
            }
            if libc::listen(listener, 1) != 0 {
                libc::close(listener);
                return None;
            }
            // Read back the bound port.
            let mut bound: libc::sockaddr_in = std::mem::zeroed();
            let mut blen = alen;
            if libc::getsockname(
                listener,
                &mut bound as *mut _ as *mut libc::sockaddr,
                &mut blen,
            ) != 0
            {
                libc::close(listener);
                return None;
            }
            let client = libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0);
            if client < 0 {
                libc::close(listener);
                return None;
            }
            if libc::connect(client, &bound as *const _ as *const libc::sockaddr, blen) != 0 {
                libc::close(client);
                libc::close(listener);
                return None;
            }
            let server = libc::accept(listener, std::ptr::null_mut(), std::ptr::null_mut());
            libc::close(listener);
            if server < 0 {
                libc::close(client);
                return None;
            }
            Some((client, server))
        }
    }

    fn is_unsupported(e: &io::Error) -> bool {
        matches!(
            e.raw_os_error(),
            Some(libc::ENOSYS) | Some(libc::EOPNOTSUPP) | Some(libc::EINVAL)
        )
    }

    #[test]
    fn send_recv_roundtrip() {
        let ring = match NetworkRing::new(16) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("send_recv_roundtrip: ring unavailable: {e}; skipping");
                return;
            }
        };
        let Some((a, b)) = socketpair() else {
            eprintln!("send_recv_roundtrip: socketpair failed; skipping");
            return;
        };
        let payload: [u8; 11] = *b"hello uring";

        let sent = {
            let fut = ring.send(a, payload.to_vec());
            let mut fut = Box::pin(fut);
            match block_on(&ring, fut.as_mut()) {
                Ok(n) => n,
                Err(e) if is_unsupported(&e) => {
                    eprintln!("send_recv_roundtrip: SEND unsupported; skipping");
                    unsafe {
                        libc::close(a);
                        libc::close(b);
                    }
                    return;
                }
                Err(e) => panic!("send failed: {e}"),
            }
        };
        assert_eq!(sent, payload.len());

        let got = {
            let fut = ring.recv(b, 64);
            let mut fut = Box::pin(fut);
            match block_on(&ring, fut.as_mut()) {
                Ok(v) => v,
                Err(e) if is_unsupported(&e) => {
                    eprintln!("send_recv_roundtrip: RECV unsupported; skipping");
                    unsafe {
                        libc::close(a);
                        libc::close(b);
                    }
                    return;
                }
                Err(e) => panic!("recv failed: {e}"),
            }
        };
        assert_eq!(got.as_slice(), &payload[..]);

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    /// SEND_ZC over a registered backing, received via recv_fixed into a
    /// disjoint region of the same backing. Explicitly asserts the
    /// two-CQE path: the send future resolves only after a F_MORE
    /// completion was recorded (i.e. `more_completions() >= 1`).
    ///
    /// Uses a TCP loopback pair because AF_UNIX sockets do not support
    /// zerocopy send (SEND_ZC returns EOPNOTSUPP there); TCP exercises
    /// the real F_MORE + notification protocol.
    #[test]
    fn send_zc_recv_fixed_two_cqe_roundtrip() {
        let ring = match NetworkRing::new(16) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("send_zc_recv_fixed: ring unavailable: {e}; skipping");
                return;
            }
        };

        const PAGE: usize = 4096;
        // Two pages: send region at offset 0, recv region at offset PAGE.
        let mut store = vec![0u8; PAGE * 2];
        let payload: &[u8] = b"zero-copy send path bytes";
        store[..payload.len()].copy_from_slice(payload);
        let backing = crate::memory::Backing {
            base: store.as_mut_ptr(),
            page_size: PAGE,
            page_count: 2,
            keepalive: std::sync::Arc::new(()),
        };
        if let Err(e) = ring.register_backing(&backing) {
            eprintln!("send_zc_recv_fixed: register_backing failed: {e}; skipping");
            return;
        }

        let Some((a, b)) = tcp_loopback_pair() else {
            eprintln!("send_zc_recv_fixed: tcp loopback pair failed; skipping");
            return;
        };

        let send_fut = ring.send_zc_fixed(a, 0, 0, payload.len());
        let recv_fut = ring.recv_fixed(b, 0, PAGE, payload.len());
        let mut send_fut = Box::pin(send_fut);
        let mut recv_fut = Box::pin(recv_fut);

        // Drive both, but record whether the send future resolved *only
        // after* a F_MORE completion had been observed. Because SEND_ZC's
        // op slot is resolved exclusively on the final (no-F_MORE)
        // notification CQE, `more_completions() >= 1` at the instant the
        // send future becomes Ready proves the two-CQE protocol executed.
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut send_res: Option<io::Result<usize>> = None;
        let mut recv_res: Option<io::Result<usize>> = None;
        let mut more_at_send_ready: u64 = 0;
        let mut spins = 0u32;
        loop {
            if send_res.is_none() {
                if let Poll::Ready(v) = send_fut.as_mut().poll(&mut cx) {
                    more_at_send_ready = ring.more_completions();
                    send_res = Some(v);
                }
            }
            if recv_res.is_none() {
                if let Poll::Ready(v) = recv_fut.as_mut().poll(&mut cx) {
                    recv_res = Some(v);
                }
            }
            ring.progress();
            if send_res.is_some() && recv_res.is_some() {
                break;
            }
            spins += 1;
            assert!(
                spins < 5_000_000,
                "send_zc_recv_fixed spun without progress"
            );
        }

        let sent = match send_res.unwrap() {
            Ok(n) => n,
            Err(e) if is_unsupported(&e) => {
                eprintln!("send_zc_recv_fixed: SEND_ZC unsupported; skipping");
                unsafe {
                    libc::close(a);
                    libc::close(b);
                }
                return;
            }
            Err(e) => panic!("send_zc failed: {e}"),
        };
        let got = match recv_res.unwrap() {
            Ok(n) => n,
            Err(e) if is_unsupported(&e) => {
                eprintln!("send_zc_recv_fixed: RECV unsupported; skipping");
                unsafe {
                    libc::close(a);
                    libc::close(b);
                }
                return;
            }
            Err(e) => panic!("recv_fixed failed: {e}"),
        };

        assert_eq!(sent, payload.len());
        assert_eq!(got, payload.len());
        assert_eq!(&store[PAGE..PAGE + payload.len()], payload);
        // The SEND_ZC op only resolves on its final (no-F_MORE)
        // notification CQE, and a F_MORE completion must have preceded
        // it: this pins the two-CQE protocol.
        assert!(
            more_at_send_ready >= 1,
            "SEND_ZC must record a F_MORE completion before its notification resolves the op",
        );

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    /// `NetHandle::recv_fixed` (the `'static` owned-future variant)
    /// receives into a registered backing page. Bytes are sent from a
    /// plain socket and must land at the requested page byte offset in
    /// the backing, proving the fixed-buffer destination is resolved and
    /// driven correctly through the owned-future path.
    #[test]
    fn handle_recv_fixed_lands_at_offset() {
        let ring = match NetworkRing::new(16) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("handle_recv_fixed: ring unavailable: {e}; skipping");
                return;
            }
        };

        const PAGE: usize = 4096;
        let mut store = vec![0u8; PAGE * 2];
        let payload: &[u8] = b"nethandle recv_fixed bytes";
        let backing = crate::memory::Backing {
            base: store.as_mut_ptr(),
            page_size: PAGE,
            page_count: 2,
            keepalive: std::sync::Arc::new(()),
        };
        if let Err(e) = ring.register_backing(&backing) {
            eprintln!("handle_recv_fixed: register_backing failed: {e}; skipping");
            return;
        }

        let ring = Rc::new(ring);
        let handle = NetHandle::new(Rc::clone(&ring));

        let Some((a, b)) = tcp_loopback_pair() else {
            eprintln!("handle_recv_fixed: tcp loopback pair failed; skipping");
            return;
        };

        // Push the payload from the peer socket directly; the recv side
        // lands it into page 1 (offset PAGE) of the registered backing.
        let wrote =
            unsafe { libc::write(a, payload.as_ptr() as *const libc::c_void, payload.len()) };
        if wrote < 0 {
            eprintln!("handle_recv_fixed: write failed; skipping");
            unsafe {
                libc::close(a);
                libc::close(b);
            }
            return;
        }
        assert_eq!(wrote as usize, payload.len());

        let got = {
            let fut = handle.recv_fixed(b, 0, PAGE, payload.len());
            let mut fut = Box::pin(fut);
            match block_on_handle(fut.as_mut()) {
                Ok(n) => n,
                Err(e) if is_unsupported(&e) => {
                    eprintln!("handle_recv_fixed: RECV unsupported; skipping");
                    unsafe {
                        libc::close(a);
                        libc::close(b);
                    }
                    return;
                }
                Err(e) => panic!("handle recv_fixed failed: {e}"),
            }
        };

        assert_eq!(got, payload.len());
        assert_eq!(&store[PAGE..PAGE + payload.len()], payload);

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    /// Dropping an in-flight `recv_fixed` future must drain the RECV to
    /// completion, not merely best-effort cancel it: the destination page
    /// can be reused the instant the future drops, so the kernel must be
    /// done writing first. We start a RECV with no data available (it
    /// parks in the kernel), drop the future, and assert the ring has no
    /// in-flight ops afterwards (the slot was reaped via ASYNC_CANCEL).
    #[test]
    fn drop_in_flight_recv_fixed_drains_the_op() {
        let ring = match NetworkRing::new(16) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("drop_in_flight_recv_fixed: ring unavailable: {e}; skipping");
                return;
            }
        };

        const PAGE: usize = 4096;
        let mut store = vec![0u8; PAGE * 2];
        let backing = crate::memory::Backing {
            base: store.as_mut_ptr(),
            page_size: PAGE,
            page_count: 2,
            keepalive: std::sync::Arc::new(()),
        };
        if let Err(e) = ring.register_backing(&backing) {
            eprintln!("drop_in_flight_recv_fixed: register_backing failed: {e}; skipping");
            return;
        }

        let ring = Rc::new(ring);
        let handle = NetHandle::new(Rc::clone(&ring));

        let Some((a, b)) = tcp_loopback_pair() else {
            eprintln!("drop_in_flight_recv_fixed: tcp loopback pair failed; skipping");
            return;
        };

        // No bytes are written to `a`, so the RECV on `b` blocks in the
        // kernel and the future parks Pending after submitting the op.
        let fut = handle.recv_fixed(b, 0, PAGE, PAGE);
        let mut fut = Box::pin(fut);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(_) => {
                // Completed/errored without blocking (e.g. RECV
                // unsupported). Nothing in flight to drain; skip.
                unsafe {
                    libc::close(a);
                    libc::close(b);
                }
                return;
            }
            Poll::Pending => {}
        }
        assert_eq!(
            ring.core.in_flight(),
            1,
            "the RECV should be outstanding after the first poll",
        );

        // Dropping the parked future must block until the RECV is reaped.
        drop(fut);
        assert_eq!(
            ring.core.in_flight(),
            0,
            "dropping a draining recv_fixed must reap the in-flight op",
        );

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    /// With a quarantine sink installed, dropping a parked fixed RECV
    /// must NOT block to drain the op: the destination page is handed to
    /// the sink (quarantined) and only reclaimed once the cancelled
    /// RECV's CQE is later reaped by `progress()`. This is the Phase 5
    /// soundness guarantee: the page is withheld from reuse for the whole
    /// window the kernel might still write to it, without a blocking drop.
    #[test]
    fn drop_in_flight_recv_fixed_quarantines_until_reaped() {
        #[derive(Clone)]
        struct TestQ {
            events: Rc<RefCell<Vec<(&'static str, usize)>>>,
        }
        impl RecvQuarantine for TestQ {
            fn quarantine(&self, page_byte_offset: usize) {
                self.events
                    .borrow_mut()
                    .push(("quarantine", page_byte_offset));
            }

            fn reclaim(&self, page_byte_offset: usize) {
                self.events.borrow_mut().push(("reclaim", page_byte_offset));
            }
        }

        let ring = match NetworkRing::new(16) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("quarantine_until_reaped: ring unavailable: {e}; skipping");
                return;
            }
        };

        const PAGE: usize = 4096;
        let mut store = vec![0u8; PAGE * 2];
        let backing = crate::memory::Backing {
            base: store.as_mut_ptr(),
            page_size: PAGE,
            page_count: 2,
            keepalive: std::sync::Arc::new(()),
        };
        if let Err(e) = ring.register_backing(&backing) {
            eprintln!("quarantine_until_reaped: register_backing failed: {e}; skipping");
            return;
        }

        let events = Rc::new(RefCell::new(Vec::new()));
        ring.set_recv_quarantine(Rc::new(TestQ {
            events: Rc::clone(&events),
        }));

        let ring = Rc::new(ring);
        let handle = NetHandle::new(Rc::clone(&ring));

        let Some((a, b)) = tcp_loopback_pair() else {
            eprintln!("quarantine_until_reaped: tcp loopback pair failed; skipping");
            return;
        };

        // No bytes written to `a`, so the RECV on `b` parks in the kernel.
        // The destination is page 1 (byte offset PAGE).
        let fut = handle.recv_fixed(b, 0, PAGE, PAGE);
        let mut fut = Box::pin(fut);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(_) => {
                unsafe {
                    libc::close(a);
                    libc::close(b);
                }
                return;
            }
            Poll::Pending => {}
        }
        assert_eq!(
            ring.core.in_flight(),
            1,
            "the RECV should be outstanding after the first poll",
        );

        // Dropping with a sink installed must be non-blocking: the op is
        // still in flight (not drained) and the page is quarantined, not
        // yet reclaimed.
        drop(fut);
        assert_eq!(
            ring.core.in_flight(),
            1,
            "dropping a quarantined recv_fixed must not block to drain",
        );
        assert_eq!(
            events.borrow().as_slice(),
            &[("quarantine", PAGE)],
            "the destination page must be quarantined on drop, not reclaimed",
        );

        // Pump progress() until the cancelled RECV's CQE is reaped. Only
        // then is the page reclaimed back to the free list.
        let mut spins = 0u32;
        while ring.core.in_flight() != 0 {
            ring.progress();
            spins += 1;
            assert!(spins < 5_000_000, "cancelled RECV was never reaped");
        }
        assert_eq!(
            events.borrow().as_slice(),
            &[("quarantine", PAGE), ("reclaim", PAGE)],
            "the page must be reclaimed only after its RECV CQE is reaped",
        );

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    /// proving the owned-future path (synchronous submit + owned slot +
    /// self-driven progress) is wired correctly.
    #[test]
    fn handle_send_recv_roundtrip() {
        let ring = match NetworkRing::new(16) {
            Ok(r) => Rc::new(r),
            Err(e) => {
                eprintln!("handle_send_recv_roundtrip: ring unavailable: {e}; skipping");
                return;
            }
        };
        let handle = NetHandle::new(Rc::clone(&ring));
        let Some((a, b)) = socketpair() else {
            eprintln!("handle_send_recv_roundtrip: socketpair failed; skipping");
            return;
        };
        let payload: [u8; 12] = *b"handle bytes";

        let sent = {
            let fut = handle.send(a, payload.to_vec());
            let mut fut = Box::pin(fut);
            match block_on_handle(fut.as_mut()) {
                Ok(n) => n,
                Err(e) if is_unsupported(&e) => {
                    eprintln!("handle_send_recv_roundtrip: SEND unsupported; skipping");
                    unsafe {
                        libc::close(a);
                        libc::close(b);
                    }
                    return;
                }
                Err(e) => panic!("handle send failed: {e}"),
            }
        };
        assert_eq!(sent, payload.len());

        let got = {
            let fut = handle.recv(b, 64);
            let mut fut = Box::pin(fut);
            match block_on_handle(fut.as_mut()) {
                Ok(v) => v,
                Err(e) if is_unsupported(&e) => {
                    eprintln!("handle_send_recv_roundtrip: RECV unsupported; skipping");
                    unsafe {
                        libc::close(a);
                        libc::close(b);
                    }
                    return;
                }
                Err(e) => panic!("handle recv failed: {e}"),
            }
        };
        assert_eq!(got.as_slice(), &payload[..]);

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    #[test]
    fn handle_submission_parks_when_ring_depth_is_full() {
        let ring = match NetworkRing::new(1) {
            Ok(r) => Rc::new(r),
            Err(e) => {
                eprintln!("handle_backpressure: ring unavailable: {e}; skipping");
                return;
            }
        };
        let handle = NetHandle::new(Rc::clone(&ring));
        let Some((a, b)) = socketpair() else {
            eprintln!("handle_backpressure: socketpair failed; skipping");
            return;
        };

        let mut first = Box::pin(handle.recv(b, 64));
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        match first.as_mut().poll(&mut cx) {
            Poll::Ready(Err(e)) if is_unsupported(&e) => {
                eprintln!("handle_backpressure: RECV unsupported; skipping");
                unsafe {
                    libc::close(a);
                    libc::close(b);
                }
                return;
            }
            Poll::Ready(other) => panic!("first recv unexpectedly completed: {other:?}"),
            Poll::Pending => {}
        }
        assert_eq!(ring.core.in_flight(), 1);

        let mut second = Box::pin(handle.recv(b, 64));
        match second.as_mut().poll(&mut cx) {
            Poll::Ready(v) => panic!("second recv must park, not complete: {v:?}"),
            Poll::Pending => {}
        }
        assert_eq!(
            ring.core.in_flight(),
            1,
            "parked submitter must not push past ring depth",
        );
        drop(second);

        let payload: &[u8] = b"x";
        let wrote = unsafe { libc::write(a, payload.as_ptr() as *const libc::c_void, 1) };
        assert_eq!(wrote, 1);
        let got = block_on_handle(first.as_mut()).expect("first recv should complete");
        assert_eq!(got, payload);

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    /// The `'static` handle futures self-drive `progress()` (they own a
    /// ring clone), so this needs no external ring reference.
    fn block_on_handle<F: Future>(mut fut: Pin<&mut F>) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut spins = 0u32;
        loop {
            if let Poll::Ready(v) = fut.as_mut().poll(&mut cx) {
                return v;
            }
            spins += 1;
            assert!(spins < 5_000_000, "block_on_handle spun without progress");
        }
    }

    #[test]
    fn sockaddr_ipv4_round_trips() {
        let sin = libc::sockaddr_in {
            sin_family: libc::AF_INET as libc::sa_family_t,
            sin_port: 8080u16.to_be(),
            sin_addr: libc::in_addr {
                s_addr: u32::from(std::net::Ipv4Addr::new(127, 0, 0, 1)).to_be(),
            },
            sin_zero: [0; 8],
        };
        let addr = SockAddr::from_sockaddr_in(sin);
        let dup = addr.duplicate();
        assert_eq!(
            dup.as_ipv4(),
            Some((std::net::Ipv4Addr::new(127, 0, 0, 1), 8080))
        );
    }

    /// `recv_fixed_msg` lands the body in the registered backing exactly
    /// like `recv_fixed`, and on a plaintext socket (no TLS control
    /// message) reports the default `application_data` record type.
    #[test]
    fn handle_recv_fixed_msg_lands_and_defaults_record_type() {
        let ring = match NetworkRing::new(16) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("recv_fixed_msg: ring unavailable: {e}; skipping");
                return;
            }
        };

        const PAGE: usize = 4096;
        let mut store = vec![0u8; PAGE * 2];
        let payload: &[u8] = b"nethandle recv_fixed_msg bytes";
        let backing = crate::memory::Backing {
            base: store.as_mut_ptr(),
            page_size: PAGE,
            page_count: 2,
            keepalive: std::sync::Arc::new(()),
        };
        if let Err(e) = ring.register_backing(&backing) {
            eprintln!("recv_fixed_msg: register_backing failed: {e}; skipping");
            return;
        }

        let ring = Rc::new(ring);
        let handle = NetHandle::new(Rc::clone(&ring));

        let Some((a, b)) = tcp_loopback_pair() else {
            eprintln!("recv_fixed_msg: tcp loopback pair failed; skipping");
            return;
        };

        let wrote =
            unsafe { libc::write(a, payload.as_ptr() as *const libc::c_void, payload.len()) };
        assert_eq!(wrote as usize, payload.len());

        let rec = {
            let fut = handle.recv_fixed_msg(b, PAGE, payload.len());
            let mut fut = Box::pin(fut);
            match block_on_handle(fut.as_mut()) {
                Ok(r) => r,
                Err(e) if is_unsupported(&e) => {
                    eprintln!("recv_fixed_msg: RECVMSG unsupported; skipping");
                    unsafe {
                        libc::close(a);
                        libc::close(b);
                    }
                    return;
                }
                Err(e) => panic!("recv_fixed_msg failed: {e}"),
            }
        };

        assert_eq!(rec.len, payload.len());
        assert_eq!(rec.record_type, TLS_RECORD_TYPE_APPLICATION_DATA);
        assert_eq!(rec.alert_desc, None);
        assert_eq!(&store[PAGE..PAGE + payload.len()], payload);

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    /// `poll_ready` resolves with a writable socket reporting `POLLOUT`
    /// in its `revents`, without consuming any stream bytes.
    #[test]
    fn handle_poll_ready_reports_writable() {
        let ring = match NetworkRing::new(16) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("poll_ready: ring unavailable: {e}; skipping");
                return;
            }
        };
        let ring = Rc::new(ring);
        let handle = NetHandle::new(Rc::clone(&ring));

        let Some((a, b)) = tcp_loopback_pair() else {
            eprintln!("poll_ready: tcp loopback pair failed; skipping");
            return;
        };

        let revents = {
            let fut = handle.poll_ready(b, libc::POLLOUT as u32);
            let mut fut = Box::pin(fut);
            match block_on_handle(fut.as_mut()) {
                Ok(r) => r,
                Err(e) if is_unsupported(&e) => {
                    eprintln!("poll_ready: POLL_ADD unsupported; skipping");
                    unsafe {
                        libc::close(a);
                        libc::close(b);
                    }
                    return;
                }
                Err(e) => panic!("poll_ready failed: {e}"),
            }
        };

        assert_ne!(revents & libc::POLLOUT as u32, 0);

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }

    /// `recv_msg` returns the received bytes in a fresh heap buffer and,
    /// on a plaintext socket (no TLS control message), reports the
    /// default `application_data` record type.
    #[test]
    fn handle_recv_msg_returns_heap_bytes_and_default_record_type() {
        let ring = match NetworkRing::new(16) {
            Ok(r) => r,
            Err(e) => {
                eprintln!("recv_msg: ring unavailable: {e}; skipping");
                return;
            }
        };
        let ring = Rc::new(ring);
        let handle = NetHandle::new(Rc::clone(&ring));

        let Some((a, b)) = tcp_loopback_pair() else {
            eprintln!("recv_msg: tcp loopback pair failed; skipping");
            return;
        };

        let payload: &[u8] = b"nethandle recv_msg header bytes";
        let wrote =
            unsafe { libc::write(a, payload.as_ptr() as *const libc::c_void, payload.len()) };
        assert_eq!(wrote as usize, payload.len());

        let (data, record_type) = {
            let fut = handle.recv_msg(b, 64 * 1024);
            let mut fut = Box::pin(fut);
            match block_on_handle(fut.as_mut()) {
                Ok(r) => r,
                Err(e) if is_unsupported(&e) => {
                    eprintln!("recv_msg: RECVMSG unsupported; skipping");
                    unsafe {
                        libc::close(a);
                        libc::close(b);
                    }
                    return;
                }
                Err(e) => panic!("recv_msg failed: {e}"),
            }
        };

        assert_eq!(data, payload);
        assert_eq!(record_type, TLS_RECORD_TYPE_APPLICATION_DATA);

        unsafe {
            libc::close(a);
            libc::close(b);
        }
    }
}
