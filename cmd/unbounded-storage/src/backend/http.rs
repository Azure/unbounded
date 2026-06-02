// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! HTTP origin backend. [`HttpBackend`] is the cache-miss origin tier:
//! when a read misses all the way through the P2P cache, it fetches the
//! stripe's byte range from a plaintext HTTP/1.1 origin server and
//! fills the destination bufferpool pages.
//!
//! This is the cold path, so it is deliberately simple: one TCP
//! connection per fetch (`Connection: close`, no pooling), and one heap
//! copy of the received body into the registered backing. The hot
//! serving path is zero-copy elsewhere; here correctness and legibility
//! win over avoiding the ingest copy.
//!
//! ## Address resolution and the `Host` header
//!
//! [`HttpBackend::resolve_origin`] resolves the configured `host:port`
//! endpoint to a single IPv4 [`SockAddr`] at startup (DNS at bring-up is
//! fine). IPv6-only origins are unsupported in v1 and surface as an
//! error. The `Host:` header sent on each request is rendered from the
//! resolved IPv4 address; a hostname-bearing `Host:` would require
//! carrying the original string, which v1 does not.
//!
//! ## Future optimizations (not in v1)
//!
//! - Connection pooling / keep-alive bounded by `http_concurrency` to
//!   amortize the TCP+`connect` handshake across fetches.
//! - Conditional revalidation via ETag. Per the content-addressed
//!   design the ETag should fold into the stripe key's identity (a new
//!   ETag yields a new key), rather than being tracked as separate
//!   per-stripe metadata.

use std::cell::RefCell;
use std::future::Future;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use ::http::header::{CONNECTION, HOST, RANGE};

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream};
use crate::http::{Method, ResponseHead, StatusCode, serialize_request};
use crate::ring::{NetworkRing, SockAddr};
use crate::storage::StripeReq;

use super::Backend;

/// Origin backend that fetches stripe byte ranges from a plaintext
/// HTTP/1.1 origin server into bufferpool pages.
///
/// Shard-pinned: the `Rc<RefCell<NetworkRing>>` and the raw
/// `backing_base` pointer are only ever touched on the owning shard
/// thread that built this backend. See the `unsafe impl Send + Sync`
/// below.
pub struct HttpBackend {
    socket: Rc<RefCell<NetworkRing>>,
    origin: SockAddr,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    backing_base: *mut u8,
}

// SAFETY: mirrors `crate::memory::Backing`. `HttpBackend` is
// shard-pinned: the embedder constructs it on, and only ever drives it
// from, a single pinned shard thread. The `Rc`, the `RefCell`, and the
// raw `backing_base` pointer are never shared across threads at
// runtime. The `Send + Sync` marker exists solely to satisfy the
// `Backend: Send + Sync` bound the embedder requires when it stores the
// backend in a cross-shard registry; it is not an invitation to touch
// the backend off its shard.
unsafe impl Send for HttpBackend {}
unsafe impl Sync for HttpBackend {}

impl HttpBackend {
    pub fn new(
        socket: Rc<RefCell<NetworkRing>>,
        origin: SockAddr,
        backend_id: String,
        stripe_size: u64,
        page_size: usize,
        backing_base: *mut u8,
    ) -> Self {
        Self {
            socket,
            origin,
            backend_id,
            stripe_size,
            page_size,
            backing_base,
        }
    }

    /// Resolve a `host:port` endpoint to a single IPv4 [`SockAddr`].
    ///
    /// Takes the first IPv4 address `ToSocketAddrs` yields. DNS at
    /// startup is acceptable for the origin tier. If only IPv6
    /// addresses resolve, this returns an error: v1 dials IPv4 only.
    pub fn resolve_origin(endpoint: &str) -> std::io::Result<SockAddr> {
        use std::net::{SocketAddr, ToSocketAddrs};

        let mut last_v6 = false;
        for addr in endpoint.to_socket_addrs()? {
            match addr {
                SocketAddr::V4(v4) => {
                    let sin = libc::sockaddr_in {
                        sin_family: libc::AF_INET as libc::sa_family_t,
                        sin_port: v4.port().to_be(),
                        sin_addr: libc::in_addr {
                            s_addr: u32::from(*v4.ip()).to_be(),
                        },
                        sin_zero: [0; 8],
                    };
                    return Ok(SockAddr::from_sockaddr_in(sin));
                }
                SocketAddr::V6(_) => last_v6 = true,
            }
        }
        let msg = if last_v6 {
            "http backend: origin resolved to IPv6 only; v1 dials IPv4 only"
        } else {
            "http backend: origin endpoint did not resolve to any address"
        };
        Err(std::io::Error::new(std::io::ErrorKind::Other, msg))
    }
}

impl Backend for HttpBackend {
    type Req = StripeReq;
    type Stream<'a> = HttpFetchStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        req: &'a Self::Req,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        let Some(origin) = req.origin() else {
            return HttpFetchStream::immediate_error("http backend: request missing origin");
        };
        let path = origin.origin_object_id.clone();
        let host = self
            .origin
            .as_ipv4()
            .map(|(ip, port)| format!("{ip}:{port}"))
            .unwrap_or_else(|| "origin".to_string());

        let dsts_owned = dsts.to_vec();
        let socket = Rc::clone(&self.socket);
        let origin_addr = &self.origin;
        let backing_base = self.backing_base;
        let page_size = self.page_size;

        // A length entry is not a byte range of the object; it is a
        // synthetic one-page cache entry whose payload is the object's
        // length. The sentinel `stripe_idx` would overflow
        // `absolute_range`, so this must branch before that is computed.
        if origin.is_length_entry() {
            let fut = Box::pin(fetch_length(
                socket,
                origin_addr,
                host,
                path,
                dsts_owned.clone(),
                backing_base,
                page_size,
            ));
            return HttpFetchStream::pending(fut, dsts_owned);
        }

        debug_assert!(!origin.is_length_entry());
        let (start, len) = absolute_range(origin.stripe_idx, self.stripe_size, src.offset, src.len);

        let fut = Box::pin(fetch(
            socket,
            origin_addr,
            host,
            path,
            start,
            len,
            dsts_owned.clone(),
            backing_base,
            page_size,
        ));
        HttpFetchStream::pending(fut, dsts_owned)
    }
}

/// Stream produced by [`HttpBackend::bulk_get`]. Drives one boxed fetch
/// future to completion, then yields each destination [`PageRef`] in
/// order (one per `poll_next`) followed by `None`. On a fetch error it
/// yields that error once then `None`.
pub struct HttpFetchStream<'a> {
    state: FetchState<'a>,
    /// The destination pages to yield, in order, once the fetch lands.
    delivered: Vec<PageRef>,
    next: usize,
}

enum FetchState<'a> {
    /// Fetch still running; the boxed future fills the destination
    /// pages and resolves `Ok(())` or `Err`.
    Running(Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>),
    /// Fetch landed; emit each delivered page in order.
    Delivering,
    /// A single error to emit before ending the stream.
    Failed(Option<Error>),
    /// Stream exhausted.
    Done,
}

impl<'a> HttpFetchStream<'a> {
    fn pending(
        fut: Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>,
        delivered: Vec<PageRef>,
    ) -> Self {
        Self {
            state: FetchState::Running(fut),
            delivered,
            next: 0,
        }
    }

    fn immediate_error(msg: &'static str) -> Self {
        Self {
            state: FetchState::Failed(Some(Error::from(msg))),
            delivered: Vec::new(),
            next: 0,
        }
    }
}

impl PageStream for HttpFetchStream<'_> {
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>> {
        let this = self.get_mut();
        loop {
            match &mut this.state {
                FetchState::Running(fut) => match fut.as_mut().poll(cx) {
                    Poll::Pending => return Poll::Pending,
                    Poll::Ready(Ok(())) => {
                        this.state = FetchState::Delivering;
                    }
                    Poll::Ready(Err(e)) => {
                        this.state = FetchState::Failed(Some(e));
                    }
                },
                FetchState::Delivering => {
                    if this.next >= this.delivered.len() {
                        this.state = FetchState::Done;
                        return Poll::Ready(None);
                    }
                    let page = this.delivered[this.next];
                    this.next += 1;
                    return Poll::Ready(Some(Ok(page)));
                }
                FetchState::Failed(slot) => {
                    let e = slot.take();
                    this.state = FetchState::Done;
                    return Poll::Ready(e.map(Err));
                }
                FetchState::Done => return Poll::Ready(None),
            }
        }
    }
}

/// Perform one whole origin fetch: dial the origin, send a ranged GET,
/// accumulate the response, validate it, and memcpy the body into the
/// destination pages. The destination pages are captured in
/// `delivered` so the stream can yield them after this resolves.
#[allow(clippy::too_many_arguments)]
async fn fetch(
    socket: Rc<RefCell<NetworkRing>>,
    origin: &SockAddr,
    host: String,
    path: String,
    start: u64,
    len: u64,
    dsts: Vec<PageRef>,
    backing_base: *mut u8,
    page_size: usize,
) -> Result<(), Error> {
    let total: u64 = dsts.iter().map(|p| p.len as u64).sum();
    if total != len {
        return Err(Error::from(
            "http backend: destination page lengths do not match requested range",
        ));
    }
    if len == 0 {
        // Guards the inclusive `start + len - 1` Range bound below
        // against underflow. The Pool always requests a full page, so
        // this is defensive.
        return Err(Error::from("http backend: zero-length fetch requested"));
    }

    let conn = TcpConn::open()?;
    {
        // The ring's op futures borrow `&NetworkRing`, so the shared
        // `RefCell` borrow is held across the await. That is sound here:
        // every ring method (including the shard's `progress()` tick
        // hook) takes `&self`, so concurrent shared borrows coexist; no
        // path takes `borrow_mut()` while a fetch is in flight.
        // SAFETY: SockAddr is owned by the backend for the fetch's
        // lifetime; the ring copies it into its own slot.
        let ring = socket.borrow();
        ring.connect(conn.fd, clone_sockaddr(origin))
            .await
            .map_err(io_to_err)?;
    }

    let request = format_get_request(&path, &host, start, start + len - 1)?;
    {
        let ring = socket.borrow();
        ring.send(conn.fd, request).await.map_err(io_to_err)?;
    }

    // Accumulate until the full header block has arrived. The origin
    // sends headers then body on the same stream.
    let mut buf: Vec<u8> = Vec::new();
    let (status, header_end, content_length, content_range_start) = loop {
        if let Some(h) = ResponseHead::parse(&buf)
            .map_err(|_| Error::from("http backend: malformed origin response head"))?
        {
            break (
                h.status,
                h.header_end,
                h.content_length(),
                h.content_range_start(),
            );
        }
        let chunk = recv_chunk(&socket, conn.fd).await?;
        if chunk.is_empty() {
            return Err(Error::from(
                "http backend: connection closed before response headers complete",
            ));
        }
        buf.extend_from_slice(&chunk);
    };

    if status != StatusCode::OK && status != StatusCode::PARTIAL_CONTENT {
        return Err(Error::from("http backend: origin returned non-2xx status"));
    }
    check_origin_status(status, start)?;

    // An origin that answers a different slice than we asked for would
    // silently corrupt the stripe, so reject a mismatched Content-Range.
    if let Some(cr_start) = content_range_start {
        if cr_start != start {
            return Err(Error::from(
                "http backend: origin Content-Range start does not match request",
            ));
        }
    }

    // The origin tells us how many body bytes this response carries. A
    // ranged request whose end runs past the object's EOF gets a short
    // 206 (Content-Length < the page we asked for) covering only the
    // bytes that exist; we read exactly that many and zero-fill the rest
    // of the page. The frontend clamps every served read to the object
    // length, so the padding is never handed to a client. A connection
    // that closes before the advertised length is a genuine truncation.
    let body_start = header_end;
    let body_len = match expected_body_len(status, content_length, len)? {
        Some(n) => {
            let want_end = body_start
                .checked_add(n as usize)
                .ok_or_else(|| Error::from("http backend: response length overflow"))?;
            while buf.len() < want_end {
                let chunk = recv_chunk(&socket, conn.fd).await?;
                if chunk.is_empty() {
                    return Err(Error::from("http backend: short read from origin"));
                }
                buf.extend_from_slice(&chunk);
            }
            buf.truncate(want_end);
            n as usize
        }
        None => {
            // No Content-Length advertised: read until the origin closes
            // the (Connection: close) stream, capped at the page we asked
            // for. EOF is unambiguous because v1 never reuses a socket.
            let want_end = body_start
                .checked_add(len as usize)
                .ok_or_else(|| Error::from("http backend: response length overflow"))?;
            while buf.len() < want_end {
                let chunk = recv_chunk(&socket, conn.fd).await?;
                if chunk.is_empty() {
                    break;
                }
                buf.extend_from_slice(&chunk);
            }
            buf.truncate(want_end);
            buf.len().saturating_sub(body_start)
        }
    };

    let body = &buf[body_start..body_start + body_len];
    copy_body_into_pages(body, &dsts, backing_base, page_size)?;
    Ok(())
}

/// Fill a length entry: HEAD the origin object, take its
/// `Content-Length` as the object's byte length, and write that length
/// as a little-endian `u64` into the (single) destination page. HEAD
/// has no body, so only the header block is read.
///
/// The 8 length bytes are written through the same
/// [`copy_body_into_pages`] path as the data fetch, which copies them
/// into the first page bytes and zero-fills the remainder of the
/// destination pages.
async fn fetch_length(
    socket: Rc<RefCell<NetworkRing>>,
    origin: &SockAddr,
    host: String,
    path: String,
    dsts: Vec<PageRef>,
    backing_base: *mut u8,
    page_size: usize,
) -> Result<(), Error> {
    let capacity: usize = dsts.iter().map(|p| p.len as usize).sum();
    if capacity < 8 {
        return Err(Error::from(
            "http backend: length entry destination smaller than 8 bytes",
        ));
    }

    let conn = TcpConn::open()?;
    {
        // See `fetch` for why the shared `RefCell` borrow held across
        // the await is sound (every ring method takes `&self`).
        let ring = socket.borrow();
        ring.connect(conn.fd, clone_sockaddr(origin))
            .await
            .map_err(io_to_err)?;
    }

    let request = format_head_request(&path, &host)?;
    {
        let ring = socket.borrow();
        ring.send(conn.fd, request).await.map_err(io_to_err)?;
    }

    // Accumulate until the full header block has arrived, capping the
    // header buffer so a pathological origin cannot grow it unbounded.
    const MAX_HEAD: usize = 64 * 1024;
    let mut buf: Vec<u8> = Vec::new();
    let (status, content_length) = loop {
        if let Some(h) = ResponseHead::parse(&buf)
            .map_err(|_| Error::from("http backend: malformed origin response head"))?
        {
            break (h.status, h.content_length());
        }
        if buf.len() >= MAX_HEAD {
            return Err(Error::from(
                "http backend: length HEAD response head exceeds 64 KiB",
            ));
        }
        let chunk = recv_chunk(&socket, conn.fd).await?;
        if chunk.is_empty() {
            return Err(Error::from(
                "http backend: connection closed before length HEAD headers complete",
            ));
        }
        buf.extend_from_slice(&chunk);
    };

    if status != StatusCode::OK {
        return Err(Error::from(
            "http backend: length HEAD returned non-200 status",
        ));
    }
    let length = content_length
        .ok_or_else(|| Error::from("http backend: length HEAD missing Content-Length"))?;

    let le_bytes = length.to_le_bytes();
    copy_body_into_pages(&le_bytes, &dsts, backing_base, page_size)?;
    Ok(())
}

/// Determine how many body bytes to read for this response, or `None`
/// when the origin advertised no `Content-Length` and the caller must
/// read until the `Connection: close` stream ends.
///
/// A `206` returns at most the bytes we asked for, so a `Content-Length`
/// exceeding `len` is a protocol violation. A `200` (accepted only at
/// offset 0) streams the whole object, which may be larger than one
/// page; we only want the first `len` bytes of it.
fn expected_body_len(
    status: StatusCode,
    content_length: Option<u64>,
    len: u64,
) -> Result<Option<u64>, Error> {
    let Some(cl) = content_length else {
        return Ok(None);
    };
    let n = if status == StatusCode::PARTIAL_CONTENT {
        if cl > len {
            return Err(Error::from(
                "http backend: origin Content-Length exceeds requested range",
            ));
        }
        cl
    } else {
        cl.min(len)
    };
    Ok(Some(n))
}

/// Validate the origin's response status against the requested offset.
///
/// `206` (Partial Content) is always fine. A `200` means the origin
/// ignored our `Range` and is streaming the whole object from byte 0;
/// that is only usable when we asked from offset 0, otherwise the body
/// would not begin at `start` and copying it would silently corrupt the
/// stripe. Non-2xx is rejected by the caller before this is reached.
fn check_origin_status(status: StatusCode, start: u64) -> Result<(), Error> {
    if status == StatusCode::OK && start != 0 {
        return Err(Error::from(
            "http backend: origin ignored Range (200) for a non-zero offset",
        ));
    }
    Ok(())
}

/// Copy the `body` bytes into the destination pages in order,
/// respecting each page's `len`, then zero-fill any page bytes the body
/// did not cover. Absolute byte offset of a page within the registered
/// backing is `page_idx * page_size + offset`.
///
/// A `body` shorter than the pages' total capacity is the normal
/// tail-of-object case: the origin had fewer bytes than the page we
/// requested, so the remainder is zero-filled. The frontend clamps
/// every served read to the object length, so that padding is never
/// returned to a client. A `body` larger than the pages can hold is a
/// protocol error.
fn copy_body_into_pages(
    body: &[u8],
    dsts: &[PageRef],
    backing_base: *mut u8,
    page_size: usize,
) -> Result<(), Error> {
    let capacity: usize = dsts.iter().map(|p| p.len as usize).sum();
    if body.len() > capacity {
        return Err(Error::from("http backend: over read from origin"));
    }
    let mut consumed = 0usize;
    for page in dsts {
        let n = page.len as usize;
        let avail = body.len().saturating_sub(consumed).min(n);
        let page_offset = (page.page_idx as usize)
            .checked_mul(page_size)
            .and_then(|base| base.checked_add(page.offset as usize))
            .ok_or_else(|| Error::from("http backend: page byte offset overflow"))?;
        // SAFETY: the destination addresses a page inside the
        // registered backing the embedder keeps alive for the shard's
        // lifetime; the backend is shard-pinned so no other thread
        // touches `backing_base`. `avail <= n` bytes are copied from
        // `body` (bounds-checked above) and the remaining `n - avail`
        // bytes of the page are zero-filled; the page geometry is the
        // caller's invariant (pages were carved from this backing).
        unsafe {
            let dst = backing_base.add(page_offset);
            if avail > 0 {
                std::ptr::copy_nonoverlapping(body.as_ptr().add(consumed), dst, avail);
            }
            if avail < n {
                std::ptr::write_bytes(dst.add(avail), 0, n - avail);
            }
        }
        consumed += avail;
    }
    Ok(())
}

/// Receive one chunk from `fd` through the shared ring, releasing the
/// `RefCell` borrow before awaiting so the ring's progress hook can run.
async fn recv_chunk(socket: &Rc<RefCell<NetworkRing>>, fd: RawFd) -> Result<Vec<u8>, Error> {
    const RECV_CHUNK: usize = 64 * 1024;
    let ring = socket.borrow();
    ring.recv(fd, RECV_CHUNK).await.map_err(io_to_err)
}

/// Compute the absolute origin byte range for a stripe sub-range. The
/// stripe begins at `stripe_idx * stripe_size`; `src_offset`/`src_len`
/// select bytes within that stripe. Returns `(absolute_start,
/// length)`.
fn absolute_range(stripe_idx: u64, stripe_size: u64, src_offset: u64, src_len: u32) -> (u64, u64) {
    let start = stripe_idx
        .saturating_mul(stripe_size)
        .saturating_add(src_offset);
    (start, src_len as u64)
}

/// Format a ranged HTTP/1.1 GET request. `start`/`end` are inclusive
/// byte offsets for the `Range` header. `Connection: close` keeps v1
/// simple (one connection per fetch).
fn format_get_request(path: &str, host: &str, start: u64, end: u64) -> Result<Vec<u8>, Error> {
    let req = ::http::Request::builder()
        .method(Method::GET)
        .uri(path)
        .header(HOST, host)
        .header(RANGE, format!("bytes={start}-{end}"))
        .header(CONNECTION, "close")
        .body(())
        .map_err(|_| Error::from("http backend: failed to build origin GET request"))?;
    Ok(serialize_request(&req))
}

/// Format an HTTP/1.1 HEAD request. Used by the length-entry fill path
/// to learn an object's byte length from its `Content-Length` without
/// transferring a body. No `Range` header: HEAD asks about the whole
/// object.
fn format_head_request(path: &str, host: &str) -> Result<Vec<u8>, Error> {
    let req = ::http::Request::builder()
        .method(Method::HEAD)
        .uri(path)
        .header(HOST, host)
        .header(CONNECTION, "close")
        .body(())
        .map_err(|_| Error::from("http backend: failed to build origin HEAD request"))?;
    Ok(serialize_request(&req))
}

/// Clone a [`SockAddr`] by round-tripping through its raw bytes, so a
/// fresh owned copy can be handed to the ring per `connect`.
fn clone_sockaddr(addr: &SockAddr) -> SockAddr {
    // Render then rebuild from the IPv4 parts; v1 origins are IPv4.
    match addr.as_ipv4() {
        Some((ip, port)) => {
            let sin = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: port.to_be(),
                sin_addr: libc::in_addr {
                    s_addr: u32::from(ip).to_be(),
                },
                sin_zero: [0; 8],
            };
            SockAddr::from_sockaddr_in(sin)
        }
        None => {
            // Non-IPv4 origins are rejected at resolve_origin; fall back
            // to an all-zero IPv4 address, which connect will reject.
            let sin = libc::sockaddr_in {
                sin_family: libc::AF_INET as libc::sa_family_t,
                sin_port: 0,
                sin_addr: libc::in_addr { s_addr: 0 },
                sin_zero: [0; 8],
            };
            SockAddr::from_sockaddr_in(sin)
        }
    }
}

fn io_to_err(e: std::io::Error) -> Error {
    match e.raw_os_error() {
        Some(code) => Error::Io(code),
        None => Error::transport(e),
    }
}

/// RAII wrapper around a libc TCP socket fd. The ring never creates
/// fds; the backend opens the socket with `libc::socket` and closes it
/// on drop (one connection per fetch in v1).
struct TcpConn {
    fd: RawFd,
}

impl TcpConn {
    fn open() -> Result<Self, Error> {
        // SAFETY: socket() with valid AF/type/protocol constants.
        let fd = unsafe { libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0) };
        if fd < 0 {
            return Err(io_to_err(std::io::Error::last_os_error()));
        }
        Ok(Self { fd })
    }
}

impl Drop for TcpConn {
    fn drop(&mut self) {
        // SAFETY: fd was returned by socket() and is not used after
        // this; closing it releases the kernel resource.
        unsafe {
            libc::close(self.fd);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn absolute_range_offsets_into_stripe() {
        // Stripe 0: range starts at the stripe base plus src.offset.
        assert_eq!(absolute_range(0, 4 * 1024 * 1024, 0, 4096), (0, 4096));
        assert_eq!(absolute_range(0, 4 * 1024 * 1024, 100, 200), (100, 200));

        // Stripe 3 at 4 MiB stripes: base is 12 MiB, plus the offset.
        let stripe = 4 * 1024 * 1024u64;
        assert_eq!(absolute_range(3, stripe, 0, 4096), (3 * stripe, 4096));
        assert_eq!(
            absolute_range(3, stripe, 8192, 4096),
            (3 * stripe + 8192, 4096)
        );
    }

    #[test]
    fn format_get_request_emits_request_line_and_headers() {
        let req = format_get_request("/models/llama.bin", "10.0.0.1:8080", 0, 4095).unwrap();
        let s = std::str::from_utf8(&req).unwrap();
        assert!(
            s.starts_with("GET /models/llama.bin HTTP/1.1\r\n"),
            "got: {s}"
        );
        assert!(s.contains("host: 10.0.0.1:8080\r\n"), "got: {s}");
        assert!(s.contains("range: bytes=0-4095\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn format_get_request_nonzero_range() {
        let req = format_get_request("/o", "h:1", 4096, 8191).unwrap();
        let s = std::str::from_utf8(&req).unwrap();
        assert!(s.contains("range: bytes=4096-8191\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"));
    }

    #[test]
    fn check_origin_status_rules() {
        // 206 is always acceptable, at any offset.
        assert!(check_origin_status(StatusCode::PARTIAL_CONTENT, 0).is_ok());
        assert!(check_origin_status(StatusCode::PARTIAL_CONTENT, 4096).is_ok());
        // 200 from offset 0 is the whole object, which is fine.
        assert!(check_origin_status(StatusCode::OK, 0).is_ok());
        // 200 for a non-zero offset means the origin ignored Range; using
        // it would corrupt the stripe, so it must be rejected.
        assert!(check_origin_status(StatusCode::OK, 1).is_err());
        assert!(check_origin_status(StatusCode::OK, 4096).is_err());
    }

    #[test]
    fn expected_body_len_206_uses_content_length() {
        // Short tail: origin had fewer bytes than the page we asked for.
        assert_eq!(
            expected_body_len(StatusCode::PARTIAL_CONTENT, Some(1000), 4096).unwrap(),
            Some(1000)
        );
    }

    #[test]
    fn expected_body_len_206_rejects_overlong_content_length() {
        // A 206 must not return more than we asked for.
        assert!(expected_body_len(StatusCode::PARTIAL_CONTENT, Some(5000), 4096).is_err());
    }

    #[test]
    fn expected_body_len_200_caps_at_requested_len() {
        // Whole-object 200 stream: we only want the first page.
        assert_eq!(
            expected_body_len(StatusCode::OK, Some(1_000_000), 4096).unwrap(),
            Some(4096)
        );
    }

    #[test]
    fn expected_body_len_200_short_object() {
        // Object shorter than a page: read the 500 bytes, zero-fill rest.
        assert_eq!(
            expected_body_len(StatusCode::OK, Some(500), 4096).unwrap(),
            Some(500)
        );
    }

    #[test]
    fn expected_body_len_absent_content_length_reads_to_close() {
        assert_eq!(expected_body_len(StatusCode::OK, None, 4096).unwrap(), None);
    }

    #[test]
    fn copy_body_into_pages_fills_each_page() {
        let page_size = 4096usize;
        let mut backing = vec![0u8; page_size * 3];
        let base = backing.as_mut_ptr();

        // Two pages: page 1 offset 0 len 4, page 2 offset 8 len 3.
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 8,
                len: 3,
            },
        ];
        let body = [1u8, 2, 3, 4, 5, 6, 7];
        copy_body_into_pages(&body, &dsts, base, page_size).unwrap();

        assert_eq!(&backing[page_size..page_size + 4], &[1, 2, 3, 4]);
        assert_eq!(&backing[2 * page_size + 8..2 * page_size + 11], &[5, 6, 7]);
    }

    #[test]
    fn copy_body_into_pages_zero_fills_short_body() {
        // A short body is the normal tail-of-object case: copy what the
        // origin had, zero-fill the rest of the page.
        let page_size = 4096usize;
        let mut backing = vec![0xFFu8; page_size];
        let base = backing.as_mut_ptr();
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 8,
        }];
        // Only 4 bytes for an 8-byte page: succeeds, tail is zeroed.
        copy_body_into_pages(&[1u8, 2, 3, 4], &dsts, base, page_size).unwrap();
        assert_eq!(&backing[0..8], &[1, 2, 3, 4, 0, 0, 0, 0]);
    }

    #[test]
    fn copy_body_into_pages_zero_fills_across_pages() {
        // Body ends partway through the first page; the remainder of
        // that page and the entire second page must be zeroed.
        let page_size = 4096usize;
        let mut backing = vec![0xAAu8; page_size * 3];
        let base = backing.as_mut_ptr();
        let dsts = [
            PageRef {
                page_idx: 1,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 2,
                offset: 0,
                len: 4,
            },
        ];
        copy_body_into_pages(&[9u8, 8], &dsts, base, page_size).unwrap();
        assert_eq!(&backing[page_size..page_size + 4], &[9, 8, 0, 0]);
        assert_eq!(&backing[2 * page_size..2 * page_size + 4], &[0, 0, 0, 0]);
    }

    #[test]
    fn copy_body_into_pages_rejects_over_body() {
        let page_size = 4096usize;
        let mut backing = vec![0u8; page_size];
        let base = backing.as_mut_ptr();
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4,
        }];
        // 6 bytes for a 4-byte page total: over read.
        let err = copy_body_into_pages(&[0u8; 6], &dsts, base, page_size).unwrap_err();
        assert!(matches!(err, Error::Transport(_)));
    }

    #[test]
    fn resolve_origin_parses_ipv4() {
        let addr = HttpBackend::resolve_origin("127.0.0.1:8080").expect("resolves");
        assert_eq!(
            addr.as_ipv4(),
            Some((std::net::Ipv4Addr::new(127, 0, 0, 1), 8080))
        );
    }

    #[test]
    fn resolve_origin_rejects_unparseable() {
        assert!(HttpBackend::resolve_origin("not a host:port at all").is_err());
    }

    #[test]
    fn format_head_request_emits_head_line_and_headers() {
        let req = format_head_request("/o", "h:1").unwrap();
        let s = std::str::from_utf8(&req).unwrap();
        assert!(s.starts_with("HEAD /o HTTP/1.1\r\n"), "got: {s}");
        assert!(s.contains("host: h:1\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
        assert!(!s.contains("range:"), "got: {s}");
    }

    #[test]
    fn length_bytes_written_le_into_page() {
        // The length-fill path encodes the object length as a
        // little-endian u64 through `copy_body_into_pages`, which must
        // land the 8 bytes at the page start and zero-fill the tail.
        let page_size = 4096usize;
        let mut backing = vec![0xFFu8; page_size];
        let base = backing.as_mut_ptr();
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: page_size as u32,
        }];
        copy_body_into_pages(&12345u64.to_le_bytes(), &dsts, base, page_size).unwrap();

        let mut head = [0u8; 8];
        head.copy_from_slice(&backing[0..8]);
        assert_eq!(u64::from_le_bytes(head), 12345);
        assert!(backing[8..].iter().all(|&b| b == 0), "tail not zeroed");
    }
}
