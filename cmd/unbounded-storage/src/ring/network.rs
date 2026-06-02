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
//! across shard-loop ticks. The borrowed [`NetworkRing`] methods return
//! `'_` futures for callers that keep the ring on the stack.
//!
//! ## SEND_ZC two-CQE semantics
//!
//! `send_zc_fixed` submits with `expects_more = true`. The op resolves
//! only on the SEND_ZC notification CQE (see [`RingCore`] docs), so when
//! the future returns the kernel is done with the source page.

use std::cell::RefCell;
use std::future::Future;
use std::io;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use io_uring::{opcode, types};

use super::core::{OpFut, OpResource, RingCore, RingSetup, Slot, check_res};

/// Thin owned wrapper around a `libc::sockaddr` plus its length, so
/// `connect` can hand a stable pointer to the kernel for the op's
/// duration.
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
    core: RingCore,
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
        if backing.base.is_null() || backing.page_size == 0 || backing.page_count == 0 {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
        let len = backing
            .page_size
            .checked_mul(backing.page_count)
            .ok_or_else(|| io::Error::from_raw_os_error(libc::EOVERFLOW))?;
        let idx = self.core.register_buffer(backing.base, len)?;
        debug_assert_eq!(idx, 0, "backing must be the first registered buffer");
        Ok(())
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
            let ptr = self.fixed_ptr(buf_index, page_byte_offset)?;
            let ud = self.core.alloc_user_data();
            let sqe = opcode::SendZc::new(types::Fd(fd), ptr, len as u32)
                .buf_index(Some(buf_index))
                .build()
                .user_data(ud);
            // SAFETY: `ptr` points inside the registered backing; the
            // caller holds the source page until this future resolves
            // (the SEND_ZC notification CQE).
            let slot = self.core.submit_op(sqe, true, OpResource::None).await?;
            let res = OpFut::new(&self.core, ud, slot).await;
            check_res(res).map(|n| n as usize)
        }
    }

    /// Resolve a `(buf_index, offset)` pair to a raw pointer inside the
    /// registered backing. Only `buf_index == 0` is registered.
    fn fixed_ptr(&self, buf_index: u16, page_byte_offset: usize) -> io::Result<*const u8> {
        if buf_index != 0 {
            return Err(io::Error::from_raw_os_error(libc::EINVAL));
        }
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
/// `Rc<RefCell<NetworkRing>>` clone instead of borrowing the ring: each
/// method submits synchronously inside a borrow block, then returns a
/// future that owns its own ring clone plus the op's `Rc<Slot>` and
/// re-borrows the ring inside `poll`/`drop`.
#[derive(Clone)]
pub struct NetHandle {
    ring: Rc<RefCell<NetworkRing>>,
}

impl NetHandle {
    /// Wrap a shared network ring.
    pub fn new(ring: Rc<RefCell<NetworkRing>>) -> Self {
        Self { ring }
    }

    /// `'static` counterpart of [`NetworkRing::accept`].
    pub fn accept(&self, listen_fd: RawFd) -> impl Future<Output = io::Result<RawFd>> + 'static {
        let ring = Rc::clone(&self.ring);
        async move {
            let (ud, slot) = {
                let r = ring.borrow();
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
            let (ud, slot) = {
                let r = ring.borrow();
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
            let (ud, slot) = {
                let r = ring.borrow();
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
            let (ud, slot) = {
                let r = ring.borrow();
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
            let (ud, slot) = {
                let r = ring.borrow();
                let ptr = r.fixed_ptr(buf_index, page_byte_offset)?;
                let ud = r.core.alloc_user_data();
                let sqe = opcode::SendZc::new(types::Fd(fd), ptr, len as u32)
                    .buf_index(Some(buf_index))
                    .build()
                    .user_data(ud);
                // SAFETY: `ptr` points inside the registered backing; the
                // caller holds the source page until this future resolves
                // (the SEND_ZC notification CQE).
                let slot = r.core.submit_now(sqe, true, OpResource::None)?;
                (ud, slot)
            };
            let res = OwnedNetFut::new(Rc::clone(&ring), ud, slot).await;
            check_res(res).map(|n| n as usize)
        }
    }
}

/// `'static` op future for the [`NetHandle`] path. Owns an
/// `Rc<RefCell<NetworkRing>>` clone instead of borrowing the ring, so it
/// can be stored across shard-loop ticks. The owned slot carries the
/// completion state; the ring borrow is taken transiently in `poll` (to
/// pump `progress`) and `drop` (to best-effort cancel).
struct OwnedNetFut {
    ring: Rc<RefCell<NetworkRing>>,
    user_data: u64,
    slot: Rc<Slot>,
}

impl OwnedNetFut {
    fn new(ring: Rc<RefCell<NetworkRing>>, user_data: u64, slot: Rc<Slot>) -> Self {
        Self {
            ring,
            user_data,
            slot,
        }
    }
}

impl Future for OwnedNetFut {
    type Output = i32;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<i32> {
        let this = self.get_mut();
        if this.slot.is_done() {
            return Poll::Ready(this.slot.result());
        }
        this.ring.borrow().progress();
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
            if let Ok(ring) = self.ring.try_borrow() {
                ring.core.cancel(self.user_data);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::pin::Pin;
    use std::sync::Arc;
    use std::task::{Context, Poll, Wake, Waker};

    use super::*;

    struct NoopWake;
    impl Wake for NoopWake {
        fn wake(self: Arc<Self>) {}
    }
    fn noop_waker() -> Waker {
        Arc::new(NoopWake).into()
    }

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
            _own: Box::new(()),
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

    /// `NetHandle` `'static` futures round-trip over a real socketpair,
    /// proving the owned-future path (synchronous submit + owned slot +
    /// self-driven progress) is wired correctly.
    #[test]
    fn handle_send_recv_roundtrip() {
        let ring = match NetworkRing::new(16) {
            Ok(r) => Rc::new(RefCell::new(r)),
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
}
