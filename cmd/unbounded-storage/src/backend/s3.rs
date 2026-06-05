// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! S3-compatible origin backend. [`S3Backend`] is the S3 sibling of
//! [`HttpBackend`](super::HttpBackend): it fetches a stripe's byte range
//! (or an object's length) from an S3-compatible origin over plaintext
//! HTTP/1.1 and fills the destination bufferpool pages.
//!
//! It mirrors the HTTP backend's fetch/length structure and shares its
//! cold-path simplicity (one TCP connection per fetch, one heap copy of
//! the body into the registered backing). It diverges in two ways:
//!
//! - It carries the origin **hostname** (not just a resolved IPv4) so the
//!   `Host:` header uses the real virtual-host name the bucket policy
//!   expects, while the TCP connect still dials the resolved IPv4
//!   [`SockAddr`].
//! - A `404 Not Found` from the origin maps to
//!   [`Error::OriginNotFound`](crate::bufferpool::Error::OriginNotFound)
//!   rather than the generic non-2xx error, so the pool can distinguish a
//!   missing object from a transport failure.

use std::future::Future;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::task::{Context, Poll};

use ::http::header::{CONNECTION, HOST, RANGE};

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream};
use crate::http::{Method, ResponseHead, StatusCode, serialize_request};
use crate::ring::{NetHandle, SockAddr};
use crate::storage::StripeReq;

use super::Backend;
use super::origin_ring::OriginRing;

/// Origin backend that fetches stripe byte ranges from an
/// S3-compatible origin into bufferpool pages over plaintext HTTP/1.1.
///
/// Shard-pinned: the [`OriginRing`] and the raw `backing_base` pointer
/// are only ever touched on the owning shard thread that built this
/// backend, OR (for the RPC-handler instance) on the ephemeral
/// `fabric-rpc-worker` thread that uses an [`OriginRing::WorkerLocal`]
/// ring private to that thread. See the `unsafe impl Send + Sync`
/// below.
pub struct S3Backend {
    ring: OriginRing,
    origin: SockAddr,
    /// The origin hostname used for the `Host:` header. The TCP connect
    /// uses `origin` (the resolved IPv4), but the bucket's virtual-host
    /// name must travel in `Host:`.
    host: String,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    backing_base: *mut u8,
}

// SAFETY: mirrors `HttpBackend`'s justification. `S3Backend` is
// shard-pinned: the embedder constructs it on, and only ever drives it
// from, a single pinned shard thread. The `OriginRing`, any `Rc`/
// `RefCell` it holds, and the raw `backing_base` pointer are never
// shared across threads at runtime. The `Send + Sync` marker exists
// solely to satisfy the `Backend: Send + Sync` bound the embedder
// requires when it stores the backend in a cross-shard registry; it is
// not an invitation to touch the backend off its shard.
unsafe impl Send for S3Backend {}
unsafe impl Sync for S3Backend {}

impl S3Backend {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        ring: OriginRing,
        origin: SockAddr,
        host: String,
        backend_id: String,
        stripe_size: u64,
        page_size: usize,
        backing_base: *mut u8,
    ) -> Self {
        Self {
            ring,
            origin,
            host,
            backend_id,
            stripe_size,
            page_size,
            backing_base,
        }
    }

    /// The configured `backend_id` this backend serves, i.e. the
    /// `OriginRef::backend_id` whose stripes route here.
    pub fn backend_id(&self) -> &str {
        &self.backend_id
    }

    /// Resolve a `host:port` endpoint to a single IPv4 [`SockAddr`].
    /// Delegates to [`HttpBackend::resolve_origin`](super::HttpBackend::resolve_origin),
    /// which takes the first IPv4 `ToSocketAddrs` yields and errors on
    /// IPv6-only origins (v1 dials IPv4 only). The hostname for the
    /// `Host:` header is passed separately to [`S3Backend::new`].
    pub fn resolve_origin(endpoint: &str) -> std::io::Result<SockAddr> {
        super::HttpBackend::resolve_origin(endpoint)
    }
}

impl Backend for S3Backend {
    type Req = StripeReq;
    type Stream<'a> = S3FetchStream<'a>;

    fn bulk_get<'a>(
        &'a self,
        req: &'a Self::Req,
        src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        let Some(origin) = req.origin() else {
            return S3FetchStream::immediate_error("s3 backend: request missing origin");
        };
        let path = origin.origin_object_id.clone();

        let dsts_owned = dsts.to_vec();
        let handle = match self.ring.handle() {
            Ok(h) => h,
            Err(e) => return S3FetchStream::immediate_err(io_to_err(e)),
        };
        let origin_addr = &self.origin;
        let backing_base = self.backing_base;
        let page_size = self.page_size;
        let host = self.host.clone();

        // A length entry is not a byte range of the object; it is a
        // synthetic one-page cache entry whose payload is the object's
        // length. The sentinel `stripe_idx` would overflow
        // `absolute_range`, so this must branch before that is computed.
        if origin.is_length_entry() {
            let fut = Box::pin(fetch_length(
                handle,
                origin_addr,
                host,
                path,
                dsts_owned.clone(),
                backing_base,
                page_size,
            ));
            return S3FetchStream::pending(fut, dsts_owned);
        }

        debug_assert!(!origin.is_length_entry());
        let (start, len) = absolute_range(origin.stripe_idx, self.stripe_size, src.offset, src.len);

        let fut = Box::pin(fetch(
            handle,
            origin_addr,
            host,
            path,
            start,
            len,
            dsts_owned.clone(),
            backing_base,
            page_size,
        ));
        S3FetchStream::pending(fut, dsts_owned)
    }
}

/// Stream produced by [`S3Backend::bulk_get`]. Drives one boxed fetch
/// future to completion, then yields each destination [`PageRef`] in
/// order (one per `poll_next`) followed by `None`. On a fetch error it
/// yields that error once then `None`.
pub struct S3FetchStream<'a> {
    state: FetchState<'a>,
    delivered: Vec<PageRef>,
    next: usize,
}

enum FetchState<'a> {
    Running(Pin<Box<dyn Future<Output = Result<(), Error>> + 'a>>),
    Delivering,
    Failed(Option<Error>),
    Done,
}

impl<'a> S3FetchStream<'a> {
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

    fn immediate_err(err: Error) -> Self {
        Self {
            state: FetchState::Failed(Some(err)),
            delivered: Vec::new(),
            next: 0,
        }
    }
}

impl PageStream for S3FetchStream<'_> {
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
/// destination pages.
#[allow(clippy::too_many_arguments)]
async fn fetch(
    handle: NetHandle,
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
            "s3 backend: destination page lengths do not match requested range",
        ));
    }
    if len == 0 {
        // Guards the inclusive `start + len - 1` Range bound below
        // against underflow. The Pool always requests a full page, so
        // this is defensive.
        return Err(Error::from("s3 backend: zero-length fetch requested"));
    }

    let conn = TcpConn::open()?;
    // See `HttpBackend::fetch` for why driving the ring on the current
    // thread is sound (the op futures self-pump on their own thread's
    // ring).
    handle
        .connect(conn.fd, clone_sockaddr(origin))
        .await
        .map_err(io_to_err)?;

    let request = format_get_request(&path, &host, start, start + len - 1)?;
    handle.send(conn.fd, request).await.map_err(io_to_err)?;

    let mut buf: Vec<u8> = Vec::new();
    let (status, header_end, content_length, content_range_start) = loop {
        if let Some(h) = ResponseHead::parse(&buf)
            .map_err(|_| Error::from("s3 backend: malformed origin response head"))?
        {
            break (
                h.status,
                h.header_end,
                h.content_length(),
                h.content_range_start(),
            );
        }
        let chunk = recv_chunk(&handle, conn.fd).await?;
        if chunk.is_empty() {
            return Err(Error::from(
                "s3 backend: connection closed before response headers complete",
            ));
        }
        buf.extend_from_slice(&chunk);
    };

    check_origin_status(status, start)?;

    // An origin that answers a different slice than we asked for would
    // silently corrupt the stripe, so reject a mismatched Content-Range.
    if let Some(cr_start) = content_range_start {
        if cr_start != start {
            return Err(Error::from(
                "s3 backend: origin Content-Range start does not match request",
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
    let body_len_mode = expected_body_len(status, content_length, len)?;

    // `body_cap` is the most body bytes we will accept into the pages.
    // For a known Content-Length it is that length (already validated by
    // `expected_body_len` to be <= the requested range, hence <=
    // capacity); for the close-delimited case it is the requested range,
    // which equals the page capacity. Either way it never exceeds
    // `capacity`, so the destination pages always have room.
    let capacity = pages_capacity(&dsts);
    debug_assert_eq!(capacity as u64, len, "capacity must equal requested range");
    let body_cap: usize = match body_len_mode {
        Some(n) => n as usize,
        None => len as usize,
    };
    if body_cap > capacity {
        return Err(Error::from("s3 backend: over read from origin"));
    }

    // recv_fixed targets the registered fixed backing at buf index 0,
    // whose base is `backing_base` (Phase 2 invariant). The page math
    // below produces registered byte offsets relative to that same base.

    // Body bytes that shared the header TCP segment are already in `buf`
    // past `header_end`; place them first with a page-aware memcpy. Any
    // bytes beyond `body_cap` are surplus the origin overstuffed and are
    // dropped (the old path truncated them identically).
    let leading = &buf[body_start..];
    let lead_take = leading.len().min(body_cap);
    if lead_take > 0 {
        write_slice_into_pages(&leading[..lead_take], &dsts, 0, backing_base, page_size)?;
    }
    let mut filled = lead_take;

    // Stream the remaining body straight into the destination pages. A
    // single recv never crosses a page boundary (pages are contiguous
    // within themselves but not with each other in the backing), so the
    // length is capped at the room left in the current page and at the
    // bytes still wanted.
    while filled < body_cap {
        let Some((page_byte_off, room)) = locate_in_pages(&dsts, filled, page_size)? else {
            break;
        };
        let recv_len = room.min(body_cap - filled);
        // SAFETY: `page_byte_off` addresses a page inside the registered
        // fixed backing (buf index 0, base == backing_base) that the
        // Pool reserves for this fetch across every await here; the
        // backend is shard-pinned so no other thread touches it. The
        // destination stays reserved until this future resolves.
        let n_recv = handle
            .recv_fixed(conn.fd, 0, page_byte_off, recv_len)
            .await
            .map_err(io_to_err)?;
        if n_recv == 0 {
            // EOF. With a known length this is a truncation; for the
            // close-delimited case it is the normal end of the body.
            match body_len_mode {
                Some(_) => return Err(Error::from("s3 backend: short read from origin")),
                None => break,
            }
        }
        filled += n_recv;
    }

    // Zero-fill the tail the body did not cover (short 206 / short 200 /
    // close-delimited stream that ended early). The frontend clamps
    // served reads to the object length, so this padding is never
    // returned to a client.
    zero_fill_pages_from(&dsts, filled, backing_base, page_size)?;
    Ok(())
}

/// Fill a length entry: HEAD the origin object, take its
/// `Content-Length` as the object's byte length, and write that length
/// as a little-endian `u64` into the (single) destination page.
#[allow(clippy::too_many_arguments)]
async fn fetch_length(
    handle: NetHandle,
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
            "s3 backend: length entry destination smaller than 8 bytes",
        ));
    }

    let conn = TcpConn::open()?;
    // See `HttpBackend::fetch` for why driving the ring on the current
    // thread is sound (the op futures self-pump on their own thread's
    // ring).
    handle
        .connect(conn.fd, clone_sockaddr(origin))
        .await
        .map_err(io_to_err)?;

    let request = format_head_request(&path, &host)?;
    handle.send(conn.fd, request).await.map_err(io_to_err)?;

    const MAX_HEAD: usize = 64 * 1024;
    let mut buf: Vec<u8> = Vec::new();
    let (status, content_length) = loop {
        if let Some(h) = ResponseHead::parse(&buf)
            .map_err(|_| Error::from("s3 backend: malformed origin response head"))?
        {
            break (h.status, h.content_length());
        }
        if buf.len() >= MAX_HEAD {
            return Err(Error::from(
                "s3 backend: length HEAD response head exceeds 64 KiB",
            ));
        }
        let chunk = recv_chunk(&handle, conn.fd).await?;
        if chunk.is_empty() {
            return Err(Error::from(
                "s3 backend: connection closed before length HEAD headers complete",
            ));
        }
        buf.extend_from_slice(&chunk);
    };

    if status == StatusCode::NOT_FOUND {
        return Err(Error::OriginNotFound);
    }
    if status != StatusCode::OK {
        return Err(Error::from(
            "s3 backend: length HEAD returned non-200 status",
        ));
    }
    let length = content_length
        .ok_or_else(|| Error::from("s3 backend: length HEAD missing Content-Length"))?;

    let le_bytes = length.to_le_bytes();
    copy_body_into_pages(&le_bytes, &dsts, backing_base, page_size)?;
    Ok(())
}

/// Determine how many body bytes to read for this response, or `None`
/// when the origin advertised no `Content-Length` and the caller must
/// read until the `Connection: close` stream ends.
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
                "s3 backend: origin Content-Length exceeds requested range",
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
/// that is only usable when we asked from offset 0. A `404 Not Found`
/// maps to [`Error::OriginNotFound`] so the pool can tell a missing
/// object apart from a transport failure. Other non-2xx are generic.
fn check_origin_status(status: StatusCode, start: u64) -> Result<(), Error> {
    if status == StatusCode::NOT_FOUND {
        return Err(Error::OriginNotFound);
    }
    if status != StatusCode::OK && status != StatusCode::PARTIAL_CONTENT {
        return Err(Error::from("s3 backend: origin returned non-2xx status"));
    }
    if status == StatusCode::OK && start != 0 {
        return Err(Error::from(
            "s3 backend: origin ignored Range (200) for a non-zero offset",
        ));
    }
    Ok(())
}

/// Copy the `body` bytes into the destination pages in order,
/// respecting each page's `len`, then zero-fill any page bytes the body
/// did not cover. Absolute byte offset of a page within the registered
/// backing is `page_idx * page_size + offset`.
fn copy_body_into_pages(
    body: &[u8],
    dsts: &[PageRef],
    backing_base: *mut u8,
    page_size: usize,
) -> Result<(), Error> {
    let capacity: usize = dsts.iter().map(|p| p.len as usize).sum();
    if body.len() > capacity {
        return Err(Error::from("s3 backend: over read from origin"));
    }
    let mut consumed = 0usize;
    for page in dsts {
        let n = page.len as usize;
        let avail = body.len().saturating_sub(consumed).min(n);
        let page_offset = (page.page_idx as usize)
            .checked_mul(page_size)
            .and_then(|base| base.checked_add(page.offset as usize))
            .ok_or_else(|| Error::from("s3 backend: page byte offset overflow"))?;
        // SAFETY: the destination addresses a page inside the registered
        // backing the embedder keeps alive for the shard's lifetime; the
        // backend is shard-pinned so no other thread touches
        // `backing_base`. `avail <= n` bytes are copied from `body`
        // (bounds-checked above) and the remaining `n - avail` bytes of
        // the page are zero-filled; the page geometry is the caller's
        // invariant (pages were carved from this backing).
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

/// Total destination byte capacity across `dsts`.
fn pages_capacity(dsts: &[PageRef]) -> usize {
    dsts.iter().map(|p| p.len as usize).sum()
}

/// Registered byte offset of a page within the fixed backing
/// (`page_idx * page_size + offset`). Identical to the math
/// [`copy_body_into_pages`] uses against `backing_base`.
fn page_byte_offset(page: &PageRef, page_size: usize) -> Result<usize, Error> {
    (page.page_idx as usize)
        .checked_mul(page_size)
        .and_then(|base| base.checked_add(page.offset as usize))
        .ok_or_else(|| Error::from("s3 backend: page byte offset overflow"))
}

/// Locate the destination page covering logical body offset `at` and
/// return `(registered_byte_offset, room)` where `room` is the number
/// of bytes left in that page from `at`. Returns `None` once `at`
/// reaches the pages' total capacity (no page covers it).
fn locate_in_pages(
    dsts: &[PageRef],
    at: usize,
    page_size: usize,
) -> Result<Option<(usize, usize)>, Error> {
    let mut page_start = 0usize;
    for page in dsts {
        let n = page.len as usize;
        if at < page_start + n {
            let within = at - page_start;
            let off = page_byte_offset(page, page_size)?
                .checked_add(within)
                .ok_or_else(|| Error::from("s3 backend: page byte offset overflow"))?;
            return Ok(Some((off, n - within)));
        }
        page_start += n;
    }
    Ok(None)
}

/// Page-aware memcpy of `src` into the destination pages starting at
/// logical body offset `start`, walking pages so a copy never crosses a
/// page boundary. Unlike [`copy_body_into_pages`] this does NOT
/// zero-fill the remainder; the tail is zeroed once after the whole
/// body has landed (see [`zero_fill_pages_from`]). Errors if the slice
/// would run past the pages' total capacity.
fn write_slice_into_pages(
    src: &[u8],
    dsts: &[PageRef],
    start: usize,
    backing_base: *mut u8,
    page_size: usize,
) -> Result<(), Error> {
    let end = start
        .checked_add(src.len())
        .ok_or_else(|| Error::from("s3 backend: page byte offset overflow"))?;
    if end > pages_capacity(dsts) {
        return Err(Error::from("s3 backend: over read from origin"));
    }
    let mut written = 0usize;
    while written < src.len() {
        let (off, room) = locate_in_pages(dsts, start + written, page_size)?
            .ok_or_else(|| Error::from("s3 backend: over read from origin"))?;
        let n = room.min(src.len() - written);
        // SAFETY: `off` addresses a page inside the registered backing
        // the embedder keeps alive for the shard's lifetime; the backend
        // is shard-pinned so no other thread touches `backing_base`. `n`
        // bytes fit within the located page (n <= room) and within `src`
        // (n <= src.len() - written), both bounds-checked above.
        unsafe {
            std::ptr::copy_nonoverlapping(src.as_ptr().add(written), backing_base.add(off), n);
        }
        written += n;
    }
    Ok(())
}

/// Zero-fill destination page bytes from logical offset `from` to the
/// end of the pages' total capacity, writing directly into the backing.
/// Preserves the tail-zeroing semantics of [`copy_body_into_pages`] for
/// the zero-copy streaming path.
fn zero_fill_pages_from(
    dsts: &[PageRef],
    from: usize,
    backing_base: *mut u8,
    page_size: usize,
) -> Result<(), Error> {
    let mut page_start = 0usize;
    for page in dsts {
        let n = page.len as usize;
        let page_end = page_start + n;
        if from < page_end {
            let within = from.saturating_sub(page_start);
            let off = page_byte_offset(page, page_size)?
                .checked_add(within)
                .ok_or_else(|| Error::from("s3 backend: page byte offset overflow"))?;
            // SAFETY: `off` addresses a page inside the registered
            // backing kept alive for the shard's lifetime; the backend is
            // shard-pinned. `n - within` bytes stay within this page
            // (within <= n), so the write does not escape it.
            unsafe {
                std::ptr::write_bytes(backing_base.add(off), 0, n - within);
            }
        }
        page_start = page_end;
    }
    Ok(())
}

/// Receive one chunk from `fd` through the origin ring. The
/// [`NetHandle`] recv future self-pumps its own ring's progress hook
/// while awaiting.
async fn recv_chunk(handle: &NetHandle, fd: RawFd) -> Result<Vec<u8>, Error> {
    const RECV_CHUNK: usize = 64 * 1024;
    handle.recv(fd, RECV_CHUNK).await.map_err(io_to_err)
}

/// Compute the absolute origin byte range for a stripe sub-range. The
/// stripe begins at `stripe_idx * stripe_size`; `src_offset`/`src_len`
/// select bytes within that stripe. Returns `(absolute_start, length)`.
fn absolute_range(stripe_idx: u64, stripe_size: u64, src_offset: u64, src_len: u32) -> (u64, u64) {
    let start = stripe_idx
        .saturating_mul(stripe_size)
        .saturating_add(src_offset);
    (start, src_len as u64)
}

/// Format a ranged HTTP/1.1 GET request against the S3 origin.
/// `start`/`end` are inclusive byte offsets for the `Range` header.
fn format_get_request(path: &str, host: &str, start: u64, end: u64) -> Result<Vec<u8>, Error> {
    let req = ::http::Request::builder()
        .method(Method::GET)
        .uri(path)
        .header(HOST, host)
        .header(RANGE, format!("bytes={start}-{end}"))
        .header(CONNECTION, "close")
        .body(())
        .map_err(|_| Error::from("s3 backend: failed to build origin GET request"))?;
    Ok(serialize_request(&req))
}

/// Format an HTTP/1.1 HEAD request against the S3 origin, used by the
/// length-entry fill path.
fn format_head_request(path: &str, host: &str) -> Result<Vec<u8>, Error> {
    let req = ::http::Request::builder()
        .method(Method::HEAD)
        .uri(path)
        .header(HOST, host)
        .header(CONNECTION, "close")
        .body(())
        .map_err(|_| Error::from("s3 backend: failed to build origin HEAD request"))?;
    Ok(serialize_request(&req))
}

/// Clone a [`SockAddr`] by round-tripping through its IPv4 parts, so a
/// fresh owned copy can be handed to the ring per `connect`.
fn clone_sockaddr(addr: &SockAddr) -> SockAddr {
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
        assert_eq!(absolute_range(0, 4 * 1024 * 1024, 0, 4096), (0, 4096));
        let stripe = 4 * 1024 * 1024u64;
        assert_eq!(
            absolute_range(3, stripe, 8192, 4096),
            (3 * stripe + 8192, 4096)
        );
    }

    #[test]
    fn check_origin_status_maps_404_to_origin_not_found() {
        let err = check_origin_status(StatusCode::NOT_FOUND, 0).unwrap_err();
        assert!(matches!(err, Error::OriginNotFound));
        // 404 maps regardless of offset.
        let err = check_origin_status(StatusCode::NOT_FOUND, 4096).unwrap_err();
        assert!(matches!(err, Error::OriginNotFound));
    }

    #[test]
    fn check_origin_status_keeps_http_rules() {
        // 206 always ok; 200 ok only at offset 0.
        assert!(check_origin_status(StatusCode::PARTIAL_CONTENT, 4096).is_ok());
        assert!(check_origin_status(StatusCode::OK, 0).is_ok());
        assert!(check_origin_status(StatusCode::OK, 1).is_err());
        // Other non-2xx stay generic (not OriginNotFound).
        let err = check_origin_status(StatusCode::FORBIDDEN, 0).unwrap_err();
        assert!(matches!(err, Error::Transport(_)));
    }

    #[test]
    fn get_request_has_expected_headers() {
        let req = format_get_request("/bucket/key", "s3.example.com", 0, 4095).unwrap();
        let s = std::str::from_utf8(&req).unwrap();
        assert!(s.starts_with("GET /bucket/key HTTP/1.1\r\n"), "got: {s}");
        assert!(s.contains("host: s3.example.com\r\n"), "got: {s}");
        assert!(s.contains("range: bytes=0-4095\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(!s.contains("x-amz-date"), "got: {s}");
        assert!(!s.contains("authorization"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn head_request_omits_range() {
        let req = format_head_request("/bucket/key", "s3.example.com").unwrap();
        let s = std::str::from_utf8(&req).unwrap();
        assert!(s.starts_with("HEAD /bucket/key HTTP/1.1\r\n"), "got: {s}");
        assert!(s.contains("host: s3.example.com\r\n"), "got: {s}");
        assert!(!s.contains("range:"), "got: {s}");
        assert!(!s.contains("x-amz-date"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn copy_body_into_pages_zero_fills_short_body() {
        let page_size = 4096usize;
        let mut backing = vec![0xFFu8; page_size];
        let base = backing.as_mut_ptr();
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 8,
        }];
        copy_body_into_pages(&[1u8, 2, 3, 4], &dsts, base, page_size).unwrap();
        assert_eq!(&backing[0..8], &[1, 2, 3, 4, 0, 0, 0, 0]);
    }

    #[test]
    fn resolve_origin_parses_ipv4() {
        let addr = S3Backend::resolve_origin("127.0.0.1:9000").expect("resolves");
        assert_eq!(
            addr.as_ipv4(),
            Some((std::net::Ipv4Addr::new(127, 0, 0, 1), 9000))
        );
    }

    #[test]
    fn pages_capacity_sums_page_lengths() {
        let dsts = [
            PageRef {
                page_idx: 0,
                offset: 0,
                len: 4,
            },
            PageRef {
                page_idx: 1,
                offset: 16,
                len: 7,
            },
        ];
        assert_eq!(pages_capacity(&dsts), 11);
        assert_eq!(pages_capacity(&[]), 0);
    }

    #[test]
    fn locate_in_pages_walks_to_correct_page() {
        let page_size = 4096usize;
        // page 1 offset 0 len 4, page 2 offset 8 len 3.
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
        assert_eq!(
            locate_in_pages(&dsts, 0, page_size).unwrap(),
            Some((page_size, 4))
        );
        assert_eq!(
            locate_in_pages(&dsts, 3, page_size).unwrap(),
            Some((page_size + 3, 1))
        );
        assert_eq!(
            locate_in_pages(&dsts, 4, page_size).unwrap(),
            Some((2 * page_size + 8, 3))
        );
        assert_eq!(
            locate_in_pages(&dsts, 5, page_size).unwrap(),
            Some((2 * page_size + 9, 2))
        );
        assert_eq!(locate_in_pages(&dsts, 7, page_size).unwrap(), None);
        assert_eq!(locate_in_pages(&dsts, 100, page_size).unwrap(), None);
    }

    #[test]
    fn write_slice_into_pages_fills_single_page_exactly() {
        let page_size = 4096usize;
        let mut backing = vec![0u8; page_size * 2];
        let base = backing.as_mut_ptr();
        let dsts = [PageRef {
            page_idx: 1,
            offset: 0,
            len: 4,
        }];
        write_slice_into_pages(&[1u8, 2, 3, 4], &dsts, 0, base, page_size).unwrap();
        assert_eq!(&backing[page_size..page_size + 4], &[1, 2, 3, 4]);
    }

    #[test]
    fn write_slice_into_pages_splits_across_pages() {
        let page_size = 4096usize;
        let mut backing = vec![0u8; page_size * 3];
        let base = backing.as_mut_ptr();
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
        write_slice_into_pages(&[1u8, 2, 3, 4, 5, 6, 7], &dsts, 0, base, page_size).unwrap();
        assert_eq!(&backing[page_size..page_size + 4], &[1, 2, 3, 4]);
        assert_eq!(&backing[2 * page_size + 8..2 * page_size + 11], &[5, 6, 7]);
    }

    #[test]
    fn write_slice_into_pages_assembles_in_two_calls() {
        // Leading bytes then remainder, written at increasing logical
        // offsets, must assemble contiguously across the page boundary.
        let page_size = 4096usize;
        let mut backing = vec![0u8; page_size * 3];
        let base = backing.as_mut_ptr();
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
        write_slice_into_pages(&[10u8, 20, 30], &dsts, 0, base, page_size).unwrap();
        write_slice_into_pages(&[40u8, 50, 60, 70], &dsts, 3, base, page_size).unwrap();
        assert_eq!(&backing[page_size..page_size + 4], &[10, 20, 30, 40]);
        assert_eq!(
            &backing[2 * page_size + 8..2 * page_size + 11],
            &[50, 60, 70]
        );
    }

    #[test]
    fn write_slice_into_pages_rejects_overflow() {
        let page_size = 4096usize;
        let mut backing = vec![0u8; page_size];
        let base = backing.as_mut_ptr();
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 4,
        }];
        let err = write_slice_into_pages(&[0u8; 6], &dsts, 0, base, page_size).unwrap_err();
        assert!(matches!(err, Error::Transport(_)));
        let err = write_slice_into_pages(&[0u8; 3], &dsts, 2, base, page_size).unwrap_err();
        assert!(matches!(err, Error::Transport(_)));
    }

    #[test]
    fn zero_fill_pages_from_zeros_tail_within_page() {
        let page_size = 4096usize;
        let mut backing = vec![0xFFu8; page_size];
        let base = backing.as_mut_ptr();
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: 8,
        }];
        backing[0..4].copy_from_slice(&[1, 2, 3, 4]);
        zero_fill_pages_from(&dsts, 4, base, page_size).unwrap();
        assert_eq!(&backing[0..8], &[1, 2, 3, 4, 0, 0, 0, 0]);
    }

    #[test]
    fn zero_fill_pages_from_spans_remaining_pages() {
        // `from` lands partway through the first page; the rest of that
        // page and the whole second page must be zeroed.
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
        zero_fill_pages_from(&dsts, 2, base, page_size).unwrap();
        assert_eq!(&backing[page_size..page_size + 2], &[0xAA, 0xAA]);
        assert_eq!(&backing[page_size + 2..page_size + 4], &[0, 0]);
        assert_eq!(&backing[2 * page_size..2 * page_size + 4], &[0, 0, 0, 0]);
    }

    #[test]
    fn write_then_zero_fill_matches_copy_body_into_pages() {
        // The streaming path (write leading + zero-fill tail) must land
        // bytes identically to the heap memcpy path for a short body.
        let page_size = 4096usize;
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

        let mut via_copy = vec![0xAAu8; page_size * 3];
        copy_body_into_pages(&[9u8, 8], &dsts, via_copy.as_mut_ptr(), page_size).unwrap();

        let mut via_stream = vec![0xAAu8; page_size * 3];
        let base = via_stream.as_mut_ptr();
        write_slice_into_pages(&[9u8, 8], &dsts, 0, base, page_size).unwrap();
        zero_fill_pages_from(&dsts, 2, base, page_size).unwrap();

        assert_eq!(via_copy, via_stream);
    }
}
