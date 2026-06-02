// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! HTTP serving frontend: the working replacement for the old S3 stub.
//!
//! This file owns two concerns:
//!
//! 1. **The factory** ([`HttpFrontend`]): validates a
//!    [`FrontendSpec`](crate::config::FrontendSpec) and binds a
//!    `SO_REUSEPORT` listening socket via `libc` so every shard can
//!    accept on the same port. It does not implement the `Frontend`
//!    trait: the concrete bufferpool type is only nameable in the
//!    binary (Phase E), so the binary calls this directly.
//! 2. **The serving engine** ([`HttpDriver`]): a per-shard,
//!    cooperatively-driven engine generic over the bufferpool `P`. It
//!    owns an internal future set (one persistent accept future plus
//!    one future per live connection) advanced by [`HttpDriver::progress`],
//!    which the shard loop registers as a tick hook.
//!
//! The hot serve path is: parse the request, resolve the object length
//! (cached, else an origin `HEAD`), resolve the byte range, write the
//! response head, then stream the body stripe-by-stripe out of the
//! bufferpool with zero-copy `SEND_ZC`, holding each [`PageGuard`]
//! across the send (the SEND_ZC notification is when the kernel is done
//! with the page) before advancing the stream.
//!
//! Linux-gated because serving depends on the io_uring
//! [`NetHandle`](crate::ring::NetHandle).

use std::cell::RefCell;
use std::future::Future;
use std::net::SocketAddr;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};
use std::time::Instant;

use ::http::header::{ACCEPT_RANGES, CONNECTION, CONTENT_LENGTH, CONTENT_RANGE, HOST};
use ::http::{Request, Response, StatusCode};

use crate::bufferpool::BufferPool;
use crate::config::{FrontendKind, FrontendSpec};
use crate::frontend::FrontendError;
use crate::frontend::cache::{Tick, TtlCache};
use crate::frontend::range::{ByteRange, RangeError, ResolvedRange, full_object, stripe_set};
use crate::http::{
    HttpRequest, Method, ParseError, ResponseHead, serialize_request, serialize_response_head,
};
use crate::ring::{NetHandle, SockAddr};
use crate::storage::{OriginRef, StripeReq};

/// Default TTL (milliseconds) for the per-shard object-length cache.
const DEFAULT_META_TTL_MS: u64 = 30_000;

/// Cap on the request header block we will buffer before giving up, so
/// a misbehaving client cannot make us allocate without bound.
const MAX_HEADER_BYTES: usize = 64 * 1024;

/// Per-recv chunk size for reading request heads and origin responses.
const RECV_CHUNK: usize = 64 * 1024;

/// Listen backlog for the accept socket.
const LISTEN_BACKLOG: i32 = 1024;

/// HTTP serving frontend factory. Built once per [`FrontendSpec`];
/// holds only the immutable configuration distilled from the spec.
///
/// Unlike the old S3 stub this does not implement the `Frontend` trait:
/// the per-shard [`HttpDriver`] is generic over the concrete bufferpool
/// type, which is only nameable in the binary, so Phase E binds the
/// listener via [`HttpFrontend::bind_listener`] and constructs the
/// driver directly.
pub struct HttpFrontend {
    id: String,
    bind: SocketAddr,
    backend_id: String,
    meta_ttl_ms: u64,
}

impl HttpFrontend {
    /// Construct from a [`FrontendSpec`], validating the kind and
    /// parsing the bind address. A configured `tls` block is accepted
    /// but ignored (loopback-only in v1).
    pub fn from_spec(spec: &FrontendSpec) -> Result<Self, FrontendError> {
        if spec.kind != FrontendKind::Http {
            return Err(FrontendError::UnsupportedKind("non-http frontend kind"));
        }
        let bind = spec
            .bind
            .parse::<SocketAddr>()
            .map_err(|_| FrontendError::BadBind(spec.bind.clone()))?;
        Ok(Self {
            id: spec.id.clone(),
            bind,
            backend_id: spec.backend.clone(),
            meta_ttl_ms: DEFAULT_META_TTL_MS,
        })
    }

    /// Stable identifier, matching [`FrontendSpec::id`].
    pub fn id(&self) -> &str {
        &self.id
    }

    /// The backend id this frontend resolves origin metadata and stripe
    /// fetches against.
    pub fn backend_id(&self) -> &str {
        &self.backend_id
    }

    /// The configured listen address.
    pub fn bind(&self) -> SocketAddr {
        self.bind
    }

    /// Object-length cache TTL in milliseconds.
    pub fn meta_ttl_ms(&self) -> u64 {
        self.meta_ttl_ms
    }

    /// Create, bind, and listen the per-shard accept socket with
    /// `SO_REUSEPORT` (and `SO_REUSEADDR`) so every shard accepts on the
    /// same port, returning the listening [`RawFd`]. Linux-only.
    ///
    /// The caller owns the returned fd and is responsible for closing
    /// it; [`HttpDriver`] only reads it for `accept`.
    pub fn bind_listener(&self) -> Result<RawFd, FrontendError> {
        bind_listener(self.bind)
    }
}

/// Per-shard HTTP serving engine, generic over the bufferpool `P` so
/// the concrete `ShardPool` type (nameable only in the binary) can be
/// plugged in by Phase E.
///
/// Owns its own internal future set: one persistent `accept` future and
/// one serve future per live connection. [`Self::progress`] advances
/// them with a noop waker, mirroring `ShardLoop::drive`; the shard loop
/// registers `progress` as a tick hook. The socket ring's own
/// `progress` is a separate tick hook (added by the shard bring-up), so
/// the serve/accept futures' slots get filled even though this engine
/// only polls them.
pub struct HttpDriver<P: BufferPool<Req = StripeReq> + 'static> {
    pool: Rc<P>,
    handle: NetHandle,
    listen_fd: RawFd,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    origin: SockAddr,
    origin_host: String,
    len_cache: Rc<RefCell<TtlCache<String, u64>>>,
    meta_ttl: Tick,
    epoch: Instant,
    accept_fut: Pin<Box<dyn Future<Output = std::io::Result<RawFd>>>>,
    conns: Vec<Pin<Box<dyn Future<Output = ()>>>>,
    waker: Waker,
}

impl<P: BufferPool<Req = StripeReq> + 'static> HttpDriver<P> {
    /// Build a serving engine over a bound `listen_fd`.
    ///
    /// `origin` is the resolved origin address used for `HEAD`
    /// object-length lookups; the `Host:` header is rendered from it.
    /// `stripe_size` and `page_size` come from the shard's pool
    /// geometry; `meta_ttl_ms` is the object-length cache TTL.
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        pool: Rc<P>,
        handle: NetHandle,
        listen_fd: RawFd,
        backend_id: String,
        stripe_size: u64,
        page_size: usize,
        origin: SockAddr,
        meta_ttl_ms: u64,
    ) -> Self {
        let origin_host = origin
            .as_ipv4()
            .map(|(ip, port)| format!("{ip}:{port}"))
            .unwrap_or_else(|| "origin".to_string());
        let accept_fut = Box::pin(handle.accept(listen_fd));
        Self {
            pool,
            handle,
            listen_fd,
            backend_id,
            stripe_size,
            page_size,
            origin,
            origin_host,
            len_cache: Rc::new(RefCell::new(TtlCache::new(meta_ttl_ms))),
            meta_ttl: meta_ttl_ms,
            epoch: Instant::now(),
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

        // Drain the accept future as long as it resolves; each
        // resolution spawns a serve future and rearms a new accept.
        loop {
            match self.accept_fut.as_mut().poll(&mut cx) {
                Poll::Ready(res) => {
                    busy = true;
                    if let Ok(fd) = res {
                        let serve = serve_connection(
                            Rc::clone(&self.pool),
                            self.handle.clone(),
                            fd,
                            self.backend_id.clone(),
                            self.stripe_size,
                            self.page_size,
                            self.origin.duplicate(),
                            self.origin_host.clone(),
                            Rc::clone(&self.len_cache),
                            self.meta_ttl,
                            self.epoch,
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
#[allow(clippy::too_many_arguments)]
async fn serve_connection<P: BufferPool<Req = StripeReq>>(
    pool: Rc<P>,
    handle: NetHandle,
    conn_fd: RawFd,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    origin: SockAddr,
    origin_host: String,
    len_cache: Rc<RefCell<TtlCache<String, u64>>>,
    meta_ttl: Tick,
    epoch: Instant,
) {
    let _fd = FdGuard(conn_fd);
    let _ = serve_request(
        &pool,
        &handle,
        conn_fd,
        &backend_id,
        stripe_size,
        page_size,
        &origin,
        &origin_host,
        &len_cache,
        meta_ttl,
        epoch,
    )
    .await;
}

/// The fallible serve body. Returns `Err(())` on any I/O, parse, or
/// pool error; the caller closes the fd regardless. Error responses
/// (400/405/416) are best-effort sends followed by `Ok(())`.
#[allow(clippy::too_many_arguments)]
async fn serve_request<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    handle: &NetHandle,
    conn_fd: RawFd,
    backend_id: &str,
    stripe_size: u64,
    page_size: usize,
    origin: &SockAddr,
    origin_host: &str,
    len_cache: &Rc<RefCell<TtlCache<String, u64>>>,
    meta_ttl: Tick,
    epoch: Instant,
) -> Result<(), ()> {
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
                let _ = send_all(handle, conn_fd, status_line_response(400)).await;
                return Ok(());
            }
        }
    }
    let req = HttpRequest::parse(&buf).map_err(|_| ())?;

    // 2. Only GET and HEAD are served. HEAD resolves length and builds
    // the same head as GET but never streams a body.
    let is_head = match req.method {
        Method::GET => false,
        Method::HEAD => true,
        _ => {
            let _ = send_all(handle, conn_fd, status_line_response(405)).await;
            return Ok(());
        }
    };

    // 3. Path is the request target with any query stripped.
    let path = split_query(req.target).0.to_string();

    // 4. Optional Range header.
    let range = match req.header("range") {
        Some(v) => match ByteRange::parse(v) {
            Ok(r) => Some(r),
            Err(_) => {
                let _ = send_all(handle, conn_fd, status_line_response(400)).await;
                return Ok(());
            }
        },
        None => None,
    };

    // 5. Resolve object length (cached, else origin HEAD).
    let now = now_ms(epoch);
    let cached = len_cache.borrow_mut().get(now, &path).copied();
    let len = match cached {
        Some(l) => l,
        None => {
            let l = origin_head_length(handle, origin, origin_host, &path)
                .await
                .map_err(|_| ())?;
            len_cache.borrow_mut().insert(now, path.clone(), l);
            let _ = meta_ttl;
            l
        }
    };

    // 6. Resolve the requested range against the length.
    let resolved = match range {
        None => full_object(len),
        Some(br) => match br.resolve(len) {
            Ok(r) => r,
            Err(RangeError::Unsatisfiable { object_len }) => {
                let _ = send_all(handle, conn_fd, unsatisfiable_response(object_len)).await;
                return Ok(());
            }
            Err(_) => {
                let _ = send_all(handle, conn_fd, status_line_response(400)).await;
                return Ok(());
            }
        },
    };

    // 7. Response head: 206 if the client sent a Range, else 200.
    let head = if range.is_some() {
        partial_head(resolved, len)
    } else {
        full_head(len)
    };
    send_all(handle, conn_fd, head).await?;

    // 7b. HEAD carries no body: the head (with Content-Length /
    // Content-Range) is the entire response.
    if is_head {
        return Ok(());
    }

    // 8. Stream the body stripe-by-stripe out of the bufferpool.
    for slice in stripe_set(resolved, stripe_size) {
        let origin_ref = OriginRef {
            backend_id: backend_id.to_string(),
            origin_object_id: path.clone(),
            stripe_idx: slice.stripe_idx,
        };
        let pool_req = StripeReq::new(origin_ref.stripe_key()).with_origin(origin_ref);
        let mut rs = pool
            .read(&pool_req, slice.intra_offset, slice.intra_len)
            .await
            .map_err(|_| ())?;
        while let Some(page) = rs.next_page().await {
            let page = page.map_err(|_| ())?;
            let pr = page.page_ref();
            let page_byte_offset = pr.page_idx as usize * page_size + pr.offset as usize;
            let n = pr.len as usize;
            // The PageGuard must stay alive until the SEND_ZC
            // notification (when the kernel is done with the source
            // page); awaiting here holds it across every partial send,
            // then we drop it before the next `next_page` as the stream
            // contract requires.
            //
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
/// `NetHandle::send` returns the CQE `res`, i.e. the number of bytes the
/// kernel accepted, which for TCP can be less than requested under
/// socket-send-buffer pressure. Dropping that count would silently
/// truncate the response, so this re-sends the unsent tail until the
/// whole buffer is on the wire. A returned count of 0 means the peer
/// closed and is surfaced as an error.
async fn send_all(handle: &NetHandle, fd: RawFd, bytes: Vec<u8>) -> Result<(), ()> {
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

/// Resolve an object's length by issuing a `HEAD` to the origin and
/// reading back its `Content-Length`. One connection per lookup
/// (`Connection: close`), matching the cold-path origin backend.
async fn origin_head_length(
    handle: &NetHandle,
    origin: &SockAddr,
    host: &str,
    path: &str,
) -> std::io::Result<u64> {
    let conn = open_tcp_v4()?;
    let _g = FdGuard(conn);
    handle.connect(conn, origin.duplicate()).await?;
    let request = Request::builder()
        .method(Method::HEAD)
        .uri(path)
        .header(HOST, host)
        .header(CONNECTION, "close")
        .body(())
        .map_err(|_| {
            std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "failed to build origin HEAD request",
            )
        })?;
    handle.send(conn, serialize_request(&request)).await?;

    let mut buf: Vec<u8> = Vec::new();
    loop {
        match ResponseHead::parse(&buf) {
            Ok(Some(head)) => {
                if head.status != StatusCode::OK {
                    return Err(std::io::Error::new(
                        std::io::ErrorKind::InvalidData,
                        "origin HEAD response was not 200 OK",
                    ));
                }
                return head.content_length().ok_or_else(|| {
                    std::io::Error::new(
                        std::io::ErrorKind::InvalidData,
                        "origin HEAD response missing Content-Length",
                    )
                });
            }
            Ok(None) => {}
            Err(_) => {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::InvalidData,
                    "origin HEAD response head malformed",
                ));
            }
        }
        if buf.len() > MAX_HEADER_BYTES {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "origin HEAD response head too large",
            ));
        }
        let chunk = handle.recv(conn, RECV_CHUNK).await?;
        if chunk.is_empty() {
            return Err(std::io::Error::new(
                std::io::ErrorKind::UnexpectedEof,
                "origin closed before HEAD response complete",
            ));
        }
        buf.extend_from_slice(&chunk);
    }
}

/// Split a raw request target into `(path, query)` where `query` is the
/// part after the first `?` (or empty). The path is the object-name
/// cache key.
fn split_query(target: &str) -> (&str, &str) {
    match target.split_once('?') {
        Some((p, q)) => (p, q),
        None => (target, ""),
    }
}

/// Format the `200 OK` head for serving a whole object.
fn full_head(len: u64) -> Vec<u8> {
    let resp = Response::builder()
        .status(StatusCode::OK)
        .header(CONTENT_LENGTH, len.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid full-object response head");
    serialize_response_head(&resp)
}

/// Format the `206 Partial Content` head for a resolved byte range.
/// `END` in `Content-Range` is inclusive (`resolved.end - 1`).
fn partial_head(resolved: ResolvedRange, total: u64) -> Vec<u8> {
    let start = resolved.start;
    let end_incl = resolved.end - 1;
    let clen = resolved.len();
    let resp = Response::builder()
        .status(StatusCode::PARTIAL_CONTENT)
        .header(CONTENT_RANGE, format!("bytes {start}-{end_incl}/{total}"))
        .header(CONTENT_LENGTH, clen.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid partial-content response head");
    serialize_response_head(&resp)
}

/// Format a `416 Range Not Satisfiable` head with `Content-Range: bytes
/// */LEN`.
fn unsatisfiable_response(total: u64) -> Vec<u8> {
    let resp = Response::builder()
        .status(StatusCode::RANGE_NOT_SATISFIABLE)
        .header(CONTENT_RANGE, format!("bytes */{total}"))
        .header(CONTENT_LENGTH, "0")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid unsatisfiable-range response head");
    serialize_response_head(&resp)
}

/// Format a bodyless status-line response (`Content-Length: 0`) for the
/// simple error statuses this frontend emits.
fn status_line_response(status: u16) -> Vec<u8> {
    let status = StatusCode::from_u16(status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let resp = Response::builder()
        .status(status)
        .header(CONTENT_LENGTH, "0")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid status-line response head");
    serialize_response_head(&resp)
}

/// Milliseconds since the driver's epoch, the tick unit for the
/// object-length cache.
fn now_ms(epoch: Instant) -> Tick {
    epoch.elapsed().as_millis() as Tick
}

/// Create, bind, and listen a TCP socket on `addr` with `SO_REUSEADDR`
/// and `SO_REUSEPORT`. Supports IPv4 and IPv6 binds.
fn bind_listener(addr: SocketAddr) -> Result<RawFd, FrontendError> {
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
fn open_tcp_v4() -> std::io::Result<RawFd> {
    // SAFETY: socket() with valid AF/type/protocol constants.
    let fd = unsafe { libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0) };
    if fd < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(fd)
}

/// RAII closer for a socket fd. The ring never creates fds; this owns
/// the lifecycle of fds the frontend opens with `libc`.
struct FdGuard(RawFd);

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
    use crate::bufferpool::{Error, ReadStream};
    use crate::config::TlsCfg;
    use crate::frontend::range::StripeSlice;

    fn spec(id: &str, bind: &str) -> FrontendSpec {
        FrontendSpec {
            id: id.to_string(),
            kind: FrontendKind::Http,
            bind: bind.to_string(),
            backend: "primary".to_string(),
            tls: None,
        }
    }

    #[test]
    fn from_spec_validates_kind_and_bind() {
        let f = HttpFrontend::from_spec(&spec("workload", "0.0.0.0:9000")).unwrap();
        assert_eq!(f.id(), "workload");
        assert_eq!(f.backend_id(), "primary");
        assert_eq!(f.bind(), "0.0.0.0:9000".parse().unwrap());

        let bad = HttpFrontend::from_spec(&spec("f", "not-an-addr"));
        assert!(matches!(bad, Err(FrontendError::BadBind(_))));
    }

    #[test]
    fn from_spec_accepts_tls_block_but_ignores_it() {
        let mut s = spec("f", "127.0.0.1:9000");
        s.tls = Some(TlsCfg {
            cert_path: None,
            key_path: None,
            secret_ref: Some("k8s://ns/name".into()),
        });
        assert!(HttpFrontend::from_spec(&s).is_ok());
    }

    #[test]
    fn split_query_strips_query() {
        assert_eq!(split_query("/bucket/key"), ("/bucket/key", ""));
        assert_eq!(
            split_query("/bucket/key?list-type=2&x=1"),
            ("/bucket/key", "list-type=2&x=1")
        );
        assert_eq!(split_query("/?"), ("/", ""));
    }

    #[test]
    fn full_head_exact_bytes() {
        let head = full_head(4096);
        let s = std::str::from_utf8(&head).unwrap();
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("content-length: 4096\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn partial_head_exact_bytes_inclusive_end() {
        // Resolved [0, 100) of a 1000-byte object -> bytes 0-99/1000.
        let head = partial_head(ResolvedRange { start: 0, end: 100 }, 1000);
        let s = std::str::from_utf8(&head).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 206 Partial Content\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes 0-99/1000\r\n"), "got: {s}");
        assert!(s.contains("content-length: 100\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn partial_head_mid_object() {
        // Resolved [70, 100) of 100 -> bytes 70-99/100, length 30.
        let head = partial_head(
            ResolvedRange {
                start: 70,
                end: 100,
            },
            100,
        );
        let s = std::str::from_utf8(&head).unwrap();
        assert!(s.contains("content-range: bytes 70-99/100\r\n"), "got: {s}");
        assert!(s.contains("content-length: 30\r\n"), "got: {s}");
    }

    #[test]
    fn unsatisfiable_response_exact_bytes() {
        let head = unsatisfiable_response(100);
        let s = std::str::from_utf8(&head).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 416 Range Not Satisfiable\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes */100\r\n"), "got: {s}");
        assert!(s.contains("content-length: 0\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn status_405_and_400_exact_bytes() {
        let r405 = status_line_response(405);
        let s405 = std::str::from_utf8(&r405).unwrap();
        assert!(
            s405.starts_with("HTTP/1.1 405 Method Not Allowed\r\n"),
            "got: {s405}"
        );
        assert!(s405.contains("content-length: 0\r\n"), "got: {s405}");
        assert!(s405.contains("connection: close\r\n"), "got: {s405}");
        assert!(s405.ends_with("\r\n\r\n"), "got: {s405}");

        let r400 = status_line_response(400);
        let s400 = std::str::from_utf8(&r400).unwrap();
        assert!(
            s400.starts_with("HTTP/1.1 400 Bad Request\r\n"),
            "got: {s400}"
        );
        assert!(s400.contains("content-length: 0\r\n"), "got: {s400}");
        assert!(s400.contains("connection: close\r\n"), "got: {s400}");
        assert!(s400.ends_with("\r\n\r\n"), "got: {s400}");
    }

    #[test]
    fn range_to_stripe_set_wiring_no_range() {
        // No Range header -> full object over the stripe set. A 10-byte
        // object at stripe 4 covers stripes 0,1,2.
        let resolved = full_object(10);
        let slices = stripe_set(resolved, 4);
        assert_eq!(
            slices,
            vec![
                StripeSlice {
                    stripe_idx: 0,
                    intra_offset: 0,
                    intra_len: 4
                },
                StripeSlice {
                    stripe_idx: 1,
                    intra_offset: 0,
                    intra_len: 4
                },
                StripeSlice {
                    stripe_idx: 2,
                    intra_offset: 0,
                    intra_len: 2
                },
            ]
        );
    }

    #[test]
    fn range_to_stripe_set_wiring_with_range() {
        // Range bytes=5-6 on a 10-byte object resolves to [5,7), which
        // at stripe 4 is stripe 1 intra [1,3).
        let br = ByteRange::parse("bytes=5-6").unwrap();
        let resolved = br.resolve(10).unwrap();
        assert_eq!(resolved, ResolvedRange { start: 5, end: 7 });
        let slices = stripe_set(resolved, 4);
        assert_eq!(
            slices,
            vec![StripeSlice {
                stripe_idx: 1,
                intra_offset: 1,
                intra_len: 2
            }]
        );
    }

    #[test]
    fn unsatisfiable_range_resolves_to_error() {
        // bytes=100-200 on a 100-byte object is unsatisfiable.
        let br = ByteRange::parse("bytes=100-200").unwrap();
        assert!(matches!(
            br.resolve(100),
            Err(RangeError::Unsatisfiable { object_len: 100 })
        ));
    }

    #[test]
    fn len_cache_hit_and_ttl_expiry() {
        let mut c: TtlCache<String, u64> = TtlCache::new(DEFAULT_META_TTL_MS);
        let key = "/bucket/key".to_string();
        assert!(c.get(0, &key).is_none());
        c.insert(0, key.clone(), 4096);
        assert_eq!(c.get(0, &key).copied(), Some(4096));
        // Live just before the TTL boundary, gone at/after it.
        assert_eq!(c.get(DEFAULT_META_TTL_MS - 1, &key).copied(), Some(4096));
        assert!(c.get(DEFAULT_META_TTL_MS, &key).is_none());
    }

    /// A mock pool whose `read` never constructs a `ReadStream` (that
    /// constructor is crate-internal to bufferpool): it always errors.
    /// Sufficient to wire an [`HttpDriver`] for accept-loop tests, where
    /// the serve path is not exercised against a real pool.
    struct MockPool;

    impl BufferPool for MockPool {
        type Req = StripeReq;

        async fn read<'p>(
            &'p self,
            _req: &'p StripeReq,
            _offset: u64,
            _len: u64,
        ) -> Result<ReadStream<'p>, Error> {
            Err(Error::from("mock pool has no data"))
        }
    }

    #[test]
    fn driver_idle_progress_returns_false_without_clients() {
        // Needs a real socket ring; skip gracefully when unavailable.
        let ring = match crate::ring::NetworkRing::new(16) {
            Ok(r) => Rc::new(RefCell::new(r)),
            Err(e) => {
                eprintln!("driver_idle_progress: ring unavailable: {e}; skipping");
                return;
            }
        };
        let handle = NetHandle::new(ring);
        let listen_fd = match bind_listener("127.0.0.1:0".parse().unwrap()) {
            Ok(fd) => fd,
            Err(e) => {
                eprintln!("driver_idle_progress: bind failed: {e}; skipping");
                return;
            }
        };
        let origin = {
            let sin = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: 8080u16.to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from(std::net::Ipv4Addr::new(127, 0, 0, 1)).to_be(),
                },
                sin_zero: [0; 8],
            };
            SockAddr::from_sockaddr_in(sin)
        };
        let mut driver = HttpDriver::new(
            Rc::new(MockPool),
            handle,
            listen_fd,
            "primary".to_string(),
            4 * 1024 * 1024,
            2 * 1024 * 1024,
            origin,
            DEFAULT_META_TTL_MS,
        );
        // No client has connected: accept is pending, no conns, so the
        // engine reports no work and stays idle.
        assert!(!driver.progress());
        assert_eq!(driver.conn_count(), 0);
        assert!(driver.is_idle());

        unsafe {
            libc::close(listen_fd);
        }
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
