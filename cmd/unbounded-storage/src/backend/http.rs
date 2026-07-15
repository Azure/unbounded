// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! HTTP origin backend. [`HttpBackend`] is the cache-miss origin tier:
//! when a read misses all the way through the P2P cache, it fetches the
//! stripe's byte range from an HTTP/1.1 origin server and fills the
//! destination bufferpool pages. The origin endpoint URL selects the
//! transport: `http://` is plaintext, `https://` runs a TLS 1.3
//! handshake (OpenSSL) and enables kernel TLS so the body still lands
//! zero-copy in the destination pages (the kernel decrypts in place).
//! The record-aware recv path lives in [`crate::tls`].
//!
//! Origin connections are kept alive when the response has a known,
//! fully-consumed body and the peer did not ask to close the stream. Idle
//! sockets remain in the backend's shard-local pool.
//!
//! ## Address resolution and the `Host` header
//!
//! [`HttpBackend::resolve_origin`] resolves the endpoint URL's authority
//! to a single IPv4 [`SockAddr`] at startup (DNS at bring-up is fine).
//! IPv6-only origins are unsupported in v1 and surface as an error. The
//! `Host:` header and the TLS SNI/certificate hostname are carried as
//! owned strings derived from the configured URL, not re-rendered from
//! the resolved address.
//!
//! ## Future optimizations (not in v1)
//!
//! - Conditional revalidation via ETag. Per the content-addressed
//!   design the ETag should fold into the stripe key's identity (a new
//!   ETag yields a new key), rather than being tracked as separate
//!   per-stripe metadata.

use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use ::http::header::{HOST, RANGE};

use crate::bufferpool::{BulkRef, Error, PageRef, PageStream};
use crate::http::{Method, StatusCode, response_closes_after_body, serialize_request};
use crate::ring::{NetHandle, SockAddr};
use crate::storage::{ObjectMetadata, StripeReq};
use crate::tls::TlsContext;

use super::Backend;
use super::conn::{
    OriginConnPool, body_response_reusable, head_response_reusable, send_request_read_head,
};
use super::limiter::FetchLimiter;

/// Origin backend that fetches stripe byte ranges from an HTTP/1.1
/// origin server (plaintext `http://` or kernel-TLS `https://`) into
/// bufferpool pages.
///
/// Shard-pinned: the [`NetHandle`] and raw `backing_base` pointer are
/// only ever touched on the owning shard thread that built this backend.
pub struct HttpBackend {
    handle: NetHandle,
    origin: SockAddr,
    /// Authority sent in the `Host:` header (`host` or `host:port`),
    /// derived from the configured endpoint URL.
    host: String,
    /// Hostname (no port) used for TLS SNI and certificate verification.
    /// Empty on a plaintext (`http://`) backend.
    sni_host: String,
    /// TLS context when the endpoint is `https://`; `None` for plaintext.
    /// Shared (`Rc`) across the fetch futures this backend spawns.
    tls: Option<Rc<TlsContext>>,
    backend_id: String,
    stripe_size: u64,
    page_size: usize,
    backing_base: *mut u8,
    limiter: FetchLimiter,
    conns: OriginConnPool,
}

impl HttpBackend {
    pub fn new(
        handle: NetHandle,
        origin: SockAddr,
        host: String,
        sni_host: String,
        tls: Option<Rc<TlsContext>>,
        backend_id: String,
        stripe_size: u64,
        page_size: usize,
        backing_base: *mut u8,
        http_concurrency: usize,
    ) -> Self {
        Self {
            handle,
            origin,
            host,
            sni_host,
            tls,
            backend_id,
            stripe_size,
            page_size,
            backing_base,
            limiter: FetchLimiter::new(http_concurrency),
            conns: OriginConnPool::new(http_concurrency),
        }
    }

    /// The configured `backend_id` this backend serves, i.e. the
    /// `OriginRef::backend_id` whose stripes route here.
    pub fn backend_id(&self) -> &str {
        &self.backend_id
    }

    /// Resolve a `host:port` URL value to a single IPv4 [`SockAddr`].
    ///
    /// Takes the first IPv4 address `ToSocketAddrs` yields. DNS at
    /// startup is acceptable for the origin tier. If only IPv6
    /// addresses resolve, this returns an error: v1 dials IPv4 only.
    pub fn resolve_origin(url: &str) -> std::io::Result<SockAddr> {
        use std::net::{SocketAddr, ToSocketAddrs};

        let mut last_v6 = false;
        for addr in url.to_socket_addrs()? {
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

impl HttpBackend {
    /// Owned-stream variant of [`Backend::bulk_get`].
    ///
    /// The returned [`HttpFetchStream`] borrows nothing from `self`: the
    /// origin address is `Copy`, the path/host are cloned, the
    /// destination pages are copied into an owned `Vec`, and the ring
    /// handle is owned. That makes the stream `'static`, which is what
    /// lets a [`super::registry::BackendRegistry`] hand out streams from
    /// a backend it owns through an `Rc` without borrowing the registry.
    pub fn fetch_stream(
        &self,
        req: &StripeReq,
        src: BulkRef,
        dsts: &[PageRef],
    ) -> HttpFetchStream<'static> {
        let Some(origin) = req.origin() else {
            return HttpFetchStream::immediate_error("http backend: request missing origin");
        };
        let path = origin.origin_object_id.clone();
        let host = self.host.clone();
        let sni_host = self.sni_host.clone();
        let tls = self.tls.clone();

        let dsts_owned = dsts.to_vec();
        let handle = self.handle.clone();
        let origin_addr = self.origin;
        let backing_base = self.backing_base;
        let page_size = self.page_size;
        let conns = self.conns.clone();

        // A metadata entry is not a byte range of the object; it is a
        // synthetic one-page cache entry whose payload is the object's
        // metadata. The sentinel `stripe_idx` would overflow
        // `absolute_range`, so this must branch before that is computed.
        if origin.is_metadata_entry() {
            let mut log = crate::obs::ReqLog::new("backend.http");
            log.str_field("op", "HEAD")
                .str_field("backend", self.backend_id())
                .str_field("path", &path)
                .field("pages", dsts_owned.len());
            let fut = Box::pin(crate::obs::instrument(
                log,
                crate::metrics::instrument_backend(
                    self.backend_id().to_string(),
                    page_size as u64,
                    fetch_metadata(
                        handle,
                        origin_addr,
                        host,
                        sni_host,
                        tls,
                        path,
                        dsts_owned.clone(),
                        backing_base,
                        page_size,
                        self.limiter.clone(),
                        conns,
                    ),
                ),
            ));
            return HttpFetchStream::pending(fut, dsts_owned);
        }

        debug_assert!(!origin.is_metadata_entry());
        let (start, len) = absolute_range(origin.stripe_idx, self.stripe_size, src.offset, src.len);

        let mut log = crate::obs::ReqLog::new("backend.http");
        log.str_field("op", "GET")
            .str_field("backend", self.backend_id())
            .str_field("path", &path)
            .field("off", start)
            .field("len", len)
            .field("pages", dsts_owned.len());
        let fut = Box::pin(crate::obs::instrument(
            log,
            crate::metrics::instrument_backend(
                self.backend_id().to_string(),
                len,
                fetch(
                    handle,
                    conns,
                    origin_addr,
                    host,
                    sni_host,
                    tls,
                    path,
                    start,
                    len,
                    dsts_owned.clone(),
                    backing_base,
                    page_size,
                    self.limiter.clone(),
                ),
            ),
        ));
        HttpFetchStream::pending(fut, dsts_owned)
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
        self.fetch_stream(req, src, dsts)
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
/// receive and validate the response head, then stream the body
/// directly into the destination bufferpool pages via `recv_fixed`
/// (zero-copy: no heap accumulation of the body). The destination
/// pages are captured in `delivered` so the stream can yield them after
/// this resolves.
#[allow(clippy::too_many_arguments)]
async fn fetch(
    handle: NetHandle,
    conns: OriginConnPool,
    origin: SockAddr,
    host: String,
    sni_host: String,
    tls: Option<Rc<TlsContext>>,
    path: String,
    start: u64,
    len: u64,
    dsts: Vec<PageRef>,
    backing_base: *mut u8,
    page_size: usize,
    limiter: FetchLimiter,
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

    // Bound concurrent origin work to `http_concurrency`. The permit is
    // held for the whole fetch and returned to the pool on drop.
    let _permit = limiter.acquire().await;
    let request = format_get_request(&path, &host, start, start + len - 1)?;
    let (conn, head) = send_request_read_head(
        &conns,
        &handle,
        origin,
        &tls,
        &sni_host,
        request,
        None,
        "http backend: malformed origin response head",
        "http backend: connection closed before response headers complete",
        "http backend: response head exceeds limit",
    )
    .await?;
    let fd = conn.fd();
    let is_tls = conn.is_tls();
    let status = head.status;
    let version_minor = head.version_minor;
    let header_end = head.header_end;
    let content_length = head.content_length;
    let content_range_start = head.content_range_start;
    let connection = head.connection;
    let buf = head.buf;

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
    let body_len_mode = expected_body_len(
        status,
        version_minor,
        connection.as_deref(),
        content_length,
        len,
    )?;

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
        return Err(Error::from("http backend: over read from origin"));
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
        let n_recv = crate::tls::recv_fixed(&handle, fd, is_tls, page_byte_off, recv_len).await?;
        if n_recv == 0 {
            // EOF. With a known length this is a truncation; for the
            // close-delimited case it is the normal end of the body.
            match body_len_mode {
                Some(_) => return Err(Error::from("http backend: short read from origin")),
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
    if body_response_reusable(
        version_minor,
        connection.as_deref(),
        content_length,
        body_cap,
        leading.len(),
        filled,
    ) {
        conns.put(conn);
    }
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
async fn fetch_metadata(
    handle: NetHandle,
    origin: SockAddr,
    host: String,
    sni_host: String,
    tls: Option<Rc<TlsContext>>,
    path: String,
    dsts: Vec<PageRef>,
    backing_base: *mut u8,
    page_size: usize,
    limiter: FetchLimiter,
    conns: OriginConnPool,
) -> Result<(), Error> {
    let capacity: usize = dsts.iter().map(|p| p.len as usize).sum();
    if capacity < 8 {
        return Err(Error::from(
            "http backend: length entry destination smaller than 8 bytes",
        ));
    }

    // Bound concurrent origin work to `http_concurrency` (see `fetch`).
    let _permit = limiter.acquire().await;
    let request = format_head_request(&path, &host)?;
    const MAX_HEAD: usize = 64 * 1024;
    let (conn, head) = send_request_read_head(
        &conns,
        &handle,
        origin,
        &tls,
        &sni_host,
        request,
        Some(MAX_HEAD),
        "http backend: malformed origin response head",
        "http backend: connection closed before metadata HEAD headers complete",
        "http backend: metadata HEAD response head exceeds 64 KiB",
    )
    .await?;
    let status = head.status;
    let version_minor = head.version_minor;
    let header_end = head.header_end;
    let content_length = head.content_length;
    let connection = head.connection;
    let buf = head.buf;

    if status == StatusCode::NOT_FOUND {
        return Err(Error::OriginNotFound);
    }
    if status != StatusCode::OK {
        return Err(Error::from(
            "http backend: metadata HEAD returned non-200 status",
        ));
    }
    let length = content_length
        .ok_or_else(|| Error::from("http backend: metadata HEAD missing Content-Length"))?;

    let body = ObjectMetadata::new(length).encode()?;
    copy_body_into_pages(&body, &dsts, backing_base, page_size)?;
    if head_response_reusable(version_minor, connection.as_deref(), header_end, buf.len()) {
        conns.put(conn);
    }
    Ok(())
}

/// Determine how many body bytes to read for this response, or `None`
/// when the origin advertised no `Content-Length` but its connection
/// semantics still guarantee EOF will delimit the body.
///
/// A `206` returns at most the bytes we asked for, so a `Content-Length`
/// exceeding `len` is a protocol violation. A `200` (accepted only at
/// offset 0) streams the whole object, which may be larger than one
/// page; we only want the first `len` bytes of it.
fn expected_body_len(
    status: StatusCode,
    version_minor: u8,
    connection: Option<&str>,
    content_length: Option<u64>,
    len: u64,
) -> Result<Option<u64>, Error> {
    let Some(cl) = content_length else {
        if !response_closes_after_body(version_minor, connection) {
            return Err(Error::from(
                "http backend: origin response missing Content-Length on keep-alive connection",
            ));
        }
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
/// `404` maps to [`Error::OriginNotFound`] so frontends can return a
/// not-found response instead of treating it as an opaque transport
/// failure. `206` (Partial Content) is always fine. A `200` means the
/// origin ignored our `Range` and is streaming the whole object from byte
/// 0; that is only usable when we asked from offset 0, otherwise the body
/// would not begin at `start` and copying it would silently corrupt the
/// stripe. Other statuses are rejected as origin protocol failures.
fn check_origin_status(status: StatusCode, start: u64) -> Result<(), Error> {
    if status == StatusCode::NOT_FOUND {
        return Err(Error::OriginNotFound);
    }
    if status != StatusCode::OK && status != StatusCode::PARTIAL_CONTENT {
        return Err(Error::from("http backend: origin returned non-2xx status"));
    }
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
pub(super) fn copy_body_into_pages(
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
        .ok_or_else(|| Error::from("http backend: page byte offset overflow"))
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
                .ok_or_else(|| Error::from("http backend: page byte offset overflow"))?;
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
        .ok_or_else(|| Error::from("http backend: page byte offset overflow"))?;
    if end > pages_capacity(dsts) {
        return Err(Error::from("http backend: over read from origin"));
    }
    let mut written = 0usize;
    while written < src.len() {
        let (off, room) = locate_in_pages(dsts, start + written, page_size)?
            .ok_or_else(|| Error::from("http backend: over read from origin"))?;
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
                .ok_or_else(|| Error::from("http backend: page byte offset overflow"))?;
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
/// byte offsets for the `Range` header.
fn format_get_request(path: &str, host: &str, start: u64, end: u64) -> Result<Vec<u8>, Error> {
    let req = ::http::Request::builder()
        .method(Method::GET)
        .uri(path)
        .header(HOST, host)
        .header(RANGE, format!("bytes={start}-{end}"))
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
        .body(())
        .map_err(|_| Error::from("http backend: failed to build origin HEAD request"))?;
    Ok(serialize_request(&req))
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
        assert!(!s.contains("connection:"), "got: {s}");
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
        assert!(matches!(
            check_origin_status(StatusCode::NOT_FOUND, 0),
            Err(Error::OriginNotFound)
        ));
        assert!(matches!(
            check_origin_status(StatusCode::NOT_FOUND, 4096),
            Err(Error::OriginNotFound)
        ));
        assert!(matches!(
            check_origin_status(StatusCode::INTERNAL_SERVER_ERROR, 0),
            Err(Error::Transport(_))
        ));
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
            expected_body_len(StatusCode::PARTIAL_CONTENT, 1, None, Some(1000), 4096).unwrap(),
            Some(1000)
        );
    }

    #[test]
    fn expected_body_len_206_rejects_overlong_content_length() {
        // A 206 must not return more than we asked for.
        assert!(expected_body_len(StatusCode::PARTIAL_CONTENT, 1, None, Some(5000), 4096).is_err());
    }

    #[test]
    fn expected_body_len_200_caps_at_requested_len() {
        // Whole-object 200 stream: we only want the first page.
        assert_eq!(
            expected_body_len(StatusCode::OK, 1, None, Some(1_000_000), 4096).unwrap(),
            Some(4096)
        );
    }

    #[test]
    fn expected_body_len_200_short_object() {
        // Object shorter than a page: read the 500 bytes, zero-fill rest.
        assert_eq!(
            expected_body_len(StatusCode::OK, 1, None, Some(500), 4096).unwrap(),
            Some(500)
        );
    }

    #[test]
    fn expected_body_len_absent_content_length_reads_to_close() {
        assert_eq!(
            expected_body_len(StatusCode::OK, 1, Some("close"), None, 4096).unwrap(),
            None
        );
        assert_eq!(
            expected_body_len(StatusCode::OK, 0, None, None, 4096).unwrap(),
            None
        );
    }

    #[test]
    fn expected_body_len_absent_content_length_rejects_keep_alive() {
        assert!(expected_body_len(StatusCode::OK, 1, None, None, 4096).is_err());
        assert!(expected_body_len(StatusCode::OK, 0, Some("keep-alive"), None, 4096).is_err());
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
        assert!(!s.contains("connection:"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
        assert!(!s.contains("range:"), "got: {s}");
    }

    #[test]
    fn metadata_written_into_page() {
        // The metadata-fill path encodes the object's `ObjectMetadata`
        // through `copy_body_into_pages`, which must land the payload at
        // the page start and zero-fill the tail. The page then decodes
        // back to the same metadata.
        let page_size = 4096usize;
        let mut backing = vec![0xFFu8; page_size];
        let base = backing.as_mut_ptr();
        let dsts = [PageRef {
            page_idx: 0,
            offset: 0,
            len: page_size as u32,
        }];
        let body = ObjectMetadata::new(12345).encode().unwrap();
        let body_len = body.len();
        copy_body_into_pages(&body, &dsts, base, page_size).unwrap();

        let decoded = ObjectMetadata::decode(&backing).unwrap();
        assert_eq!(decoded, ObjectMetadata::new(12345));
        assert_eq!(decoded.length, 12345);
        assert!(decoded.is_empty());
        assert!(
            backing[body_len..].iter().all(|&b| b == 0),
            "tail not zeroed"
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
