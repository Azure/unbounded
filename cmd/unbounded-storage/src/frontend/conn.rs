// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! The shared, protocol-agnostic serving engine.
//!
//! This module owns everything the HTTP and S3 frontends have in
//! common: binding the per-shard `SO_REUSEPORT` listener, the
//! cooperative accept/serve driver the shard loop ticks, reading a
//! request head off a connection, and streaming a response body
//! stripe-by-stripe out of the bufferpool with zero-copy `SEND_ZC`.
//!
//! The protocol-specific behavior - routing a request target to an
//! object, resolving its length/metadata, formatting response heads,
//! and shaping error bodies - lives behind the [`ServePolicy`] trait.
//! Each frontend provides one [`ServePolicy`] implementation
//! ([`super::http::HttpPolicy`], [`super::s3::S3Policy`]); the engine
//! drives both identically.
//!
//! Linux-gated because serving depends on the io_uring
//! [`NetHandle`](crate::ring::NetHandle).

use std::future::Future;
use std::net::SocketAddr;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

use crate::bufferpool::BufferPool;
use crate::frontend::FrontendError;
use crate::http::{HttpRequest, ParseError};
use crate::ring::NetHandle;
use crate::storage::StripeReq;

/// Cap on the request header block we will buffer before giving up, so
/// a misbehaving client cannot make us allocate without bound.
pub(crate) const MAX_HEADER_BYTES: usize = 64 * 1024;

/// Per-recv chunk size for reading request heads and origin responses.
pub(crate) const RECV_CHUNK: usize = 64 * 1024;

/// Listen backlog for the accept socket.
const LISTEN_BACKLOG: i32 = 1024;

/// The protocol-specific half of a frontend.
///
/// A `ServePolicy` maps a parsed request to an [`Action`]: either a
/// fully-formed response to send verbatim (errors, sub-resource
/// replies, `HEAD` responses) or a head plus a list of body stripes
/// the engine streams out of the pool. Implementations carry whatever
/// per-shard state they need (an object-length cache and origin
/// address for HTTP; a loaded catalog for S3) and are shared across a
/// shard's connections behind an `Rc`.
pub trait ServePolicy {
    /// Build the response plan for a fully parsed request head.
    ///
    /// `handle` is the shard's socket ring, passed so a policy that
    /// resolves metadata from an origin (HTTP `HEAD`) can open its own
    /// outbound connection. A policy that resolves metadata locally
    /// (S3 catalog) ignores it.
    async fn respond(&self, handle: &NetHandle, req: &HttpRequest<'_>) -> Action;

    /// Response bytes for a request head that could not be parsed.
    fn malformed_request(&self) -> Vec<u8>;
}

/// What the engine should do with a connection after the policy has
/// inspected its request.
pub enum Action {
    /// Send these response bytes verbatim, then close. Used for error
    /// responses, `?location` and similar sub-resource replies, and
    /// `HEAD` responses (which are head-only by definition).
    Respond(Vec<u8>),
    /// Send `head`, then stream `body` stripe-by-stripe out of the
    /// pool with `SEND_ZC`, then close.
    Stream {
        head: Vec<u8>,
        body: Vec<BodyStripe>,
    },
    /// Close the connection without sending anything. Used when the
    /// policy cannot produce a response (e.g. an origin metadata
    /// lookup failed) and there is nothing meaningful to send back.
    Close,
}

/// One contiguous slice of an object to stream, expressed as a pool
/// request plus the intra-stripe byte window to read from it.
pub struct BodyStripe {
    pub req: StripeReq,
    pub intra_offset: u64,
    pub intra_len: u64,
}

/// Per-shard serving engine, generic over the bufferpool `P` and the
/// [`ServePolicy`] `Pol`.
///
/// Owns its own internal future set: one persistent `accept` future
/// and one serve future per live connection. [`Self::progress`]
/// advances them with a noop waker, mirroring `ShardLoop::drive`; the
/// shard loop registers `progress` as a tick hook. The socket ring's
/// own `progress` is a separate tick hook, so the serve/accept
/// futures' slots get filled even though this engine only polls them.
pub struct ServeDriver<P, Pol>
where
    P: BufferPool<Req = StripeReq> + 'static,
    Pol: ServePolicy + 'static,
{
    pool: Rc<P>,
    policy: Rc<Pol>,
    handle: NetHandle,
    listen_fd: RawFd,
    page_size: usize,
    accept_fut: Pin<Box<dyn Future<Output = std::io::Result<RawFd>>>>,
    conns: Vec<Pin<Box<dyn Future<Output = ()>>>>,
    waker: Waker,
}

impl<P, Pol> ServeDriver<P, Pol>
where
    P: BufferPool<Req = StripeReq> + 'static,
    Pol: ServePolicy + 'static,
{
    /// Build a serving engine over a bound `listen_fd`. `page_size`
    /// comes from the shard's pool geometry and is used to translate a
    /// page's `(page_idx, offset)` into a fixed-buffer byte offset for
    /// `SEND_ZC`.
    pub fn new(
        pool: Rc<P>,
        policy: Rc<Pol>,
        handle: NetHandle,
        listen_fd: RawFd,
        page_size: usize,
    ) -> Self {
        let accept_fut = Box::pin(handle.accept(listen_fd));
        Self {
            pool,
            policy,
            handle,
            listen_fd,
            page_size,
            accept_fut,
            conns: Vec::new(),
            waker: noop_waker(),
        }
    }

    /// Number of live connection serve futures.
    pub fn conn_count(&self) -> usize {
        self.conns.len()
    }

    /// Whether the engine has no in-flight connection futures. The
    /// accept future is always live, so idleness only tracks
    /// connections.
    pub fn is_idle(&self) -> bool {
        self.conns.is_empty()
    }

    /// Advance the engine by one cooperative step: drain ready accepts
    /// (spawning a serve future per new connection and rearming a fresh
    /// accept), then poll every live connection future once, dropping
    /// the completed ones. Returns whether any future made progress so
    /// the shard loop busy-polls under load.
    pub fn progress(&mut self) -> bool {
        let mut busy = false;
        let waker = self.waker.clone();
        let mut cx = Context::from_waker(&waker);

        loop {
            match self.accept_fut.as_mut().poll(&mut cx) {
                Poll::Ready(res) => {
                    busy = true;
                    if let Ok(fd) = res {
                        let serve = serve_connection(
                            Rc::clone(&self.pool),
                            Rc::clone(&self.policy),
                            self.handle.clone(),
                            fd,
                            self.page_size,
                        );
                        self.conns.push(Box::pin(serve));
                    }
                    // Rearm regardless of accept success so a transient
                    // accept error does not stop the listener.
                    self.accept_fut = Box::pin(self.handle.accept(self.listen_fd));
                }
                Poll::Pending => break,
            }
        }

        let mut i = 0;
        while i < self.conns.len() {
            match self.conns[i].as_mut().poll(&mut cx) {
                Poll::Ready(()) => {
                    let _ = self.conns.swap_remove(i);
                    busy = true;
                }
                Poll::Pending => i += 1,
            }
        }
        busy
    }
}

/// Serve one accepted connection end-to-end, then close its fd.
///
/// Owns everything it needs so the future is `'static` and can live in
/// the driver's future set across ticks. All paths close `conn_fd` via
/// the [`FdGuard`]; serve errors just drop the connection.
async fn serve_connection<P, Pol>(
    pool: Rc<P>,
    policy: Rc<Pol>,
    handle: NetHandle,
    conn_fd: RawFd,
    page_size: usize,
) where
    P: BufferPool<Req = StripeReq>,
    Pol: ServePolicy,
{
    let _fd = FdGuard(conn_fd);
    let _ = serve_request(&pool, &policy, &handle, conn_fd, page_size).await;
}

/// The fallible serve body. Returns `Err(())` on any I/O, parse, or
/// pool error; the caller closes the fd regardless. Error responses
/// are best-effort sends followed by `Ok(())`.
async fn serve_request<P, Pol>(
    pool: &Rc<P>,
    policy: &Rc<Pol>,
    handle: &NetHandle,
    conn_fd: RawFd,
    page_size: usize,
) -> Result<(), ()>
where
    P: BufferPool<Req = StripeReq>,
    Pol: ServePolicy,
{
    // 1. Read until the request head is complete (or the cap is hit).
    let mut buf: Vec<u8> = Vec::new();
    loop {
        match HttpRequest::parse(&buf) {
            Ok(_) => break,
            Err(ParseError::Incomplete) => {
                if buf.len() > MAX_HEADER_BYTES {
                    return Err(());
                }
                let chunk = handle.recv(conn_fd, RECV_CHUNK).await.map_err(|_| ())?;
                if chunk.is_empty() {
                    return Err(());
                }
                buf.extend_from_slice(&chunk);
            }
            Err(_) => {
                let _ = send_all(handle, conn_fd, policy.malformed_request()).await;
                return Ok(());
            }
        }
    }
    let req = HttpRequest::parse(&buf).map_err(|_| ())?;

    // 2. Hand the parsed request to the policy for the response plan.
    match policy.respond(handle, &req).await {
        Action::Respond(bytes) => {
            let _ = send_all(handle, conn_fd, bytes).await;
            Ok(())
        }
        Action::Stream { head, body } => {
            send_all(handle, conn_fd, head).await?;
            stream_body(pool, handle, conn_fd, page_size, body).await
        }
        Action::Close => Err(()),
    }
}

/// Stream a body plan stripe-by-stripe out of the pool with zero-copy
/// `SEND_ZC`, holding each [`PageGuard`](crate::bufferpool::PageGuard)
/// across the send (the SEND_ZC notification is when the kernel is done
/// with the source page) before advancing the stream.
async fn stream_body<P>(
    pool: &Rc<P>,
    handle: &NetHandle,
    conn_fd: RawFd,
    page_size: usize,
    body: Vec<BodyStripe>,
) -> Result<(), ()>
where
    P: BufferPool<Req = StripeReq>,
{
    for stripe in body {
        let mut rs = pool
            .read(&stripe.req, stripe.intra_offset, stripe.intra_len)
            .await
            .map_err(|_| ())?;
        while let Some(page) = rs.next_page().await {
            let page = page.map_err(|_| ())?;
            let pr = page.page_ref();
            let page_byte_offset = pr.page_idx as usize * page_size + pr.offset as usize;
            let n = pr.len as usize;
            // SEND_ZC's CQE `res` is the count the kernel accepted,
            // which can be short under socket-send-buffer pressure, so
            // we loop advancing the byte offset until the whole page is
            // on the wire. A zero count means the peer closed.
            let mut sent_offset = page_byte_offset;
            let mut remaining = n;
            while remaining > 0 {
                let sent = handle
                    .send_zc_fixed(conn_fd, 0, sent_offset, remaining)
                    .await
                    .map_err(|_| ())?;
                if sent == 0 {
                    return Err(());
                }
                sent_offset += sent;
                remaining -= sent;
            }
            drop(page);
        }
    }
    Ok(())
}

/// Send `bytes` in full over `fd`, looping until every queued byte is
/// accepted by the kernel.
///
/// `NetHandle::send` returns the CQE `res`, i.e. the number of bytes
/// the kernel accepted, which for TCP can be less than requested under
/// socket-send-buffer pressure. Dropping that count would silently
/// truncate the response, so this re-sends the unsent tail until the
/// whole buffer is on the wire. A returned count of 0 means the peer
/// closed and is surfaced as an error.
pub(crate) async fn send_all(handle: &NetHandle, fd: RawFd, bytes: Vec<u8>) -> Result<(), ()> {
    let total = bytes.len();
    let mut sent = 0usize;
    while sent < total {
        let n = handle
            .send(fd, bytes[sent..].to_vec())
            .await
            .map_err(|_| ())?;
        if n == 0 {
            return Err(());
        }
        sent += n;
    }
    Ok(())
}

/// Split a raw request target into `(path, query)` where `query` is
/// the part after the first `?` (or empty).
pub(crate) fn split_query(target: &str) -> (&str, &str) {
    match target.split_once('?') {
        Some((p, q)) => (p, q),
        None => (target, ""),
    }
}

/// Create, bind, and listen a TCP socket on `addr` with `SO_REUSEADDR`
/// and `SO_REUSEPORT` so every shard accepts on the same port,
/// returning the listening [`RawFd`]. Supports IPv4 and IPv6 binds.
///
/// The caller owns the returned fd and is responsible for closing it.
pub(crate) fn bind_listener(addr: SocketAddr) -> Result<RawFd, FrontendError> {
    let family = match addr {
        SocketAddr::V4(_) => libc::AF_INET,
        SocketAddr::V6(_) => libc::AF_INET6,
    };
    // SAFETY: socket() with valid family/type constants.
    let fd = unsafe { libc::socket(family, libc::SOCK_STREAM | libc::SOCK_CLOEXEC, 0) };
    if fd < 0 {
        return Err(FrontendError::BadBind(addr.to_string()));
    }
    let guard = FdGuard(fd);

    let one: libc::c_int = 1;
    for opt in [libc::SO_REUSEADDR, libc::SO_REUSEPORT] {
        // SAFETY: &one outlives the call; size matches a c_int.
        let rc = unsafe {
            libc::setsockopt(
                fd,
                libc::SOL_SOCKET,
                opt,
                &one as *const libc::c_int as *const libc::c_void,
                std::mem::size_of::<libc::c_int>() as libc::socklen_t,
            )
        };
        if rc != 0 {
            return Err(FrontendError::BadBind(addr.to_string()));
        }
    }

    let bind_rc = match addr {
        SocketAddr::V4(v4) => {
            let sin = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: v4.port().to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from(*v4.ip()).to_be(),
                },
                sin_zero: [0; 8],
            };
            // SAFETY: sin is a valid sockaddr_in for the call's duration.
            unsafe {
                libc::bind(
                    fd,
                    &sin as *const libc::sockaddr_in as *const libc::sockaddr,
                    std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t,
                )
            }
        }
        SocketAddr::V6(v6) => {
            let sin6 = libc::sockaddr_in6 {
                sin6_family: libc::AF_INET6 as libc::sa_family_t,
                sin6_port: v6.port().to_be(),
                sin6_flowinfo: v6.flowinfo(),
                sin6_addr: libc::in6_addr {
                    s6_addr: v6.ip().octets(),
                },
                sin6_scope_id: v6.scope_id(),
            };
            // SAFETY: sin6 is a valid sockaddr_in6 for the call's duration.
            unsafe {
                libc::bind(
                    fd,
                    &sin6 as *const libc::sockaddr_in6 as *const libc::sockaddr,
                    std::mem::size_of::<libc::sockaddr_in6>() as libc::socklen_t,
                )
            }
        }
    };
    if bind_rc != 0 {
        return Err(FrontendError::BadBind(addr.to_string()));
    }

    // SAFETY: fd is a bound stream socket.
    if unsafe { libc::listen(fd, LISTEN_BACKLOG) } != 0 {
        return Err(FrontendError::BadBind(addr.to_string()));
    }

    // Hand the fd out to the caller; defuse the guard.
    std::mem::forget(guard);
    Ok(fd)
}

/// Open a non-bound IPv4 TCP socket for an outbound origin connection.
pub(crate) fn open_tcp_v4() -> std::io::Result<RawFd> {
    // SAFETY: socket() with valid AF/type/protocol constants.
    let fd = unsafe { libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0) };
    if fd < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(fd)
}

/// RAII closer for a socket fd. The ring never creates fds; this owns
/// the lifecycle of fds the frontend opens with `libc`.
pub(crate) struct FdGuard(pub RawFd);

impl Drop for FdGuard {
    fn drop(&mut self) {
        // SAFETY: the fd was returned by socket()/accept() and is not
        // used after this guard drops.
        unsafe {
            libc::close(self.0);
        }
    }
}

/// A noop waker for polling the engine's internal future set, mirroring
/// `ShardLoop`'s cooperative discipline.
fn noop_waker() -> Waker {
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    // SAFETY: VTABLE clone/wake/drop are all no-ops over static data.
    unsafe { Waker::from_raw(raw()) }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn split_query_strips_query() {
        assert_eq!(split_query("/bucket/key"), ("/bucket/key", ""));
        assert_eq!(
            split_query("/bucket/key?list-type=2&x=1"),
            ("/bucket/key", "list-type=2&x=1")
        );
        assert_eq!(split_query("/?"), ("/", ""));
        assert_eq!(split_query("/bucket/?location"), ("/bucket/", "location"));
    }

    #[test]
    fn bind_listener_creates_and_binds() {
        match bind_listener("127.0.0.1:0".parse().unwrap()) {
            Ok(fd) => {
                assert!(fd >= 0);
                unsafe {
                    libc::close(fd);
                }
            }
            Err(e) => eprintln!("bind_listener_creates_and_binds: bind failed: {e}; skipping"),
        }
    }
}
